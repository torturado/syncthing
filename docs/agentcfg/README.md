# agentcfg

Checks and corrects the AI coding agents on a machine against a declared configuration, so
every machine in a personal fleet runs the same MCP servers, no hook or plugin appears without
you knowing, and no credential sits in a file that gets synced or committed.

It reads only the configuration that applies machine-wide and leaves agent state alone:
project trust, session history and per-repository `.mcp.json` files are never reported or
written.

Supported agents: Claude Code and Codex CLI.

## Install

```sh
go install github.com/torturado/agentcfg/cmd/agentcfg@latest
```

## Usage

```sh
agentcfg check                  # reads ~/.config/agentcfg/spec.toml
agentcfg check --spec path.toml --json
agentcfg apply                  # lists the changes apply would make
agentcfg apply --write          # makes them
```

`--host name` selects a `[hosts]` entry instead of matching this machine's names.

### Exit status

| Code | `check` | `apply` |
|---|---|---|
| `0` | the machine matches the spec | nothing to change, or the changes were written |
| `1` | at least one error | changes pending (run without `--write`) |
| `2` | the spec or a configuration file could not be read | same, or a file could not be written |

`--json` prints a report with a `schema_version` (currently `1`). The version changes only
when a field is removed or changes meaning; fields may be added.

## Spec

The top level is the base every machine shares. `[roles.*]` and the `[hosts.*]` entry that
matches this machine add to it, in that order: a server replaces one with the same name, and
hooks and plugins accumulate. A host matches by hostname (with or without domain) or tailnet
name, so unifying the fleet does not erase what one machine needs on its own. Once the spec has
`[hosts]` entries, a machine that matches none is refused instead of checked against the base
alone, which would make `apply` remove what that machine adds.

```toml
[mcp_servers.search]
url = "https://search.example.com/mcp"
headers = { "X-API-Key" = "${SEARCH_API_KEY}" }

[mcp_servers.local-files]
command = "mcp-files"
args = ["--root", "~/notes"]
agents = ["claude"]      # default: every supported agent

[hooks]
claude = ["bash ~/.claude/hooks/notify.sh"]   # allowed hook commands; omitted agent = none

[plugins]
claude = ["formatter@official"]               # allowed Claude Code plugins

[roles.build]
mcp_servers.registry = { url = "https://registry.example.ts.net/mcp" }

[hosts.aimery]
roles = ["build"]
age_recipient = "age1..."                     # this machine may decrypt secrets.age
hooks = { claude = ["python3 ~/.claude/hooks/gpu-guard.py"] }
```

A credential in the spec must be a reference such as `${NAME}`; the spec is rejected
otherwise. For Codex, a header must be exactly `${VAR}` (written as `env_http_headers`) or
`Bearer ${VAR}` on `Authorization` (written as `bearer_token_env_var`), and an environment
variable can only pass through under its own name.

A hook is named by its command followed by its arguments, as reported by `check`. Plugin
hooks keep `${CLAUDE_PLUGIN_ROOT}` unexpanded, so the same name works on every machine.

## What `check` reports

| Rule | Severity | Meaning |
|---|---|---|
| `missing-server` | error | A declared server is not configured or not enabled. |
| `server-mismatch` | error | A declared server points at a different URL or command, or its headers, environment or arguments differ from the spec. |
| `undeclared-server` | error | An enabled server is not in the spec, in any scope, including one a plugin brings. |
| `undeclared-hook` | error | A hook is not in the spec, such as one reinstalled by another tool or carried by a plugin. |
| `undeclared-plugin` | error | A Claude Code plugin is enabled machine-wide but not in the spec. |
| `unexpanded-variable` | error | A Claude Code settings `env` value references `${VAR}`, which Claude Code passes on as written. |
| `literal-secret` | error | A credential header, token or variable is written in the file instead of referenced. |
| `shadowed-server` | warning | The same server is defined in several scopes. |
| `unset-variable` | warning | A referenced variable is not set where the check ran, so the server may start unauthenticated. |

Secret values are never read into the report: a field only records whether it is written in
the file, which variables it references and a digest to compare it with the spec. URLs and hook
commands are reported with what may be a credential replaced by `REDACTED`: user information,
query values, the value after a credential name (`Authorization: …`, `api_key=…`,
`--token …`) and Bearer or Basic tokens.

## What `apply` writes

`apply` owns only these keys, and carries every other key over unchanged:

| File | Owned |
|---|---|
| `~/.claude.json` | `mcpServers` (the user scope). Never `projects`. |
| `~/.mcp.json` | `mcpServers` approved in settings: declared ones are corrected there, undeclared ones removed. Unapproved entries, which never load, are kept. |
| `~/.claude/settings.json` | Hooks not in the spec are removed. Permissions, env and the rest are kept. |
| `~/.codex/config.toml` | The `mcp_servers` tables it changes, and inline hook events. Never `projects` or the `hooks.state` trust store. Comments and other tables are kept. |
| `~/.codex/hooks.json` | Hooks not in the spec are removed. |

A declared server enabled in `~/.mcp.json` is corrected there, and one enabled with the same
target in managed settings is left there; neither is added to `~/.claude.json`, though an entry
already there is corrected too, since the user scope also applies outside the home directory. Within a
server, only the keys the spec describes are written (URL, command, arguments, headers,
environment), so options such as a Codex `startup_timeout_sec`, and comments in its table, are
kept. An undeclared Codex server with `enabled = false` is left alone, as `check` does. Hooks are only removed, never added, because
the spec lists the hooks allowed, not where they go. Plugins and managed files are reported but
never changed.

Writes are atomic (temporary file and rename). A symlink is kept: the file it points to is
written, with its mode. Before each run's first write the previous content is copied to
`$XDG_STATE_HOME/agentcfg/backups/<time>/`, and the last 20 runs are kept. If an agent rewrites
a file while `apply` runs, `apply` plans again from the new content instead of overwriting it.
Running `apply` twice changes nothing the second time.

## Secrets

Synced files hold only references (`${VAR}` for Claude, `env_http_headers` and
`bearer_token_env_var` for Codex). The values live in `secrets.age` next to the spec,
encrypted to the `age_recipient` of every host, so the file can travel through the same synced
folder.

```sh
agentcfg secrets keygen                 # once per machine; prints its age_recipient
agentcfg secrets set COOLIFY_TOKEN      # value read from stdin; re-encrypts to every host
                                        # (--create for the fleet's first secret)
agentcfg secrets unset COOLIFY_TOKEN
agentcfg secrets rekey                  # after adding or removing a host's age_recipient
agentcfg secrets sync                   # decrypts into ~/.config/environment.d/agentcfg.conf
```

The identity is `~/.config/agentcfg/identity.txt` (mode 0600, never synced). `sync` writes a
mode 0600 file of `NAME='value'` lines that systemd user units read through `environment.d`
(after `systemctl --user daemon-reload`, or at the next login) and that login shells read with:

```sh
set -a; . ~/.config/environment.d/agentcfg.conf; set +a
```

To keep both readers in agreement, a value may not contain whitespace, quotes, `$`, a backslash
or a backtick.

A missing `secrets.age` is an error for every subcommand, so a machine the synced file has not
reached yet neither empties its environment file nor, with `set`, replaces the fleet's secrets
with a new file. Only `set --create` starts a new one.

### Moving an existing machine to references

1. `agentcfg secrets keygen`, add the printed `age_recipient` to the machine's `[hosts]` entry,
   then `agentcfg secrets rekey` from a machine that can already decrypt.
2. `agentcfg secrets set NAME` for each credential (with `--create` for the first one, when the
   fleet has no `secrets.age` yet), then `agentcfg secrets sync` on every machine.
3. Add the `headers` or `env` references to the spec's servers. Without them, `apply` would
   write the servers without credentials.
4. Remove the literal entries from what `apply` does not own (the `env` of `settings.json`,
   servers in local project scopes), then `agentcfg apply --write` and `agentcfg check`. Claude Code does not
   expand `${VAR}` in settings `env`: drop the key and let the agent inherit the variable, or,
   for the API credential, use `apiKeyHelper`, for example
   `"apiKeyHelper": "printf %s \"$PROXY_TOKEN\""`.

## Library

```go
report, err := agentcfg.Check(ctx, agentcfg.Options{SpecPath: path})
plan, err := agentcfg.Plan(ctx, opts)    // what Apply would change
result, err := agentcfg.Apply(ctx, opts)
paths, err := agentcfg.WatchPaths(opts)  // files whose changes can change the result
```

## Where it looks

| Agent | Files |
|---|---|
| Claude Code | `~/.claude/settings.json`, `~/.claude.json` (user and local MCP scopes only), `~/.mcp.json`, `/etc/claude-code/managed-settings.json`, `managed-settings.d/`, `managed-mcp.json`, and for each plugin enabled in those settings: `plugins/installed_plugins.json`, its `hooks/hooks.json`, `.mcp.json` and the hooks or servers its `plugin.json` adds. Honors `CLAUDE_CONFIG_DIR`. |
| Codex CLI | `~/.codex/config.toml` (ignoring the `hooks.state` trust store), `~/.codex/hooks.json`, `/etc/codex/config.toml`. Honors `CODEX_HOME`. |

## Development

```sh
go test ./...
```

The fixtures in `testdata` are homes in known states: `clean` matches the spec with every
credential referenced, `drifted` reproduces common drift (hooks reinstalled by another tool, an
extra MCP server in a local scope, credentials written in the files, a changed URL), `plugin`
enables plugins that carry a hook and a server, and `fleet` is a machine with its own servers,
hooks and permissions on top of a shared base. All values in them are fake.

## License

MIT
