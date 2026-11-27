# agentcfg

Checks and corrects the AI coding agents on a machine against a declared configuration, so
every machine in a personal fleet runs the same MCP servers, no hook or plugin appears without
you knowing, and no credential sits in a file that gets synced or committed.

It reads only the configuration that applies machine-wide and leaves agent state alone:
project trust, session history and per-repository `.mcp.json` files are never reported or
written.

Supported agents: Claude Code, Codex CLI and Antigravity (the app, the IDE and the `agy` CLI,
which share `~/.gemini/config`). Antigravity is checked only on a machine where it has run,
that is, where `~/.gemini/antigravity`, `antigravity-cli` or `antigravity-ide` exists.

It lives in the Syncthing fork, which also runs it on its own: the library is
`internal/agentcfg`, the command `cmd/agentcfg`, and the Syncthing service `lib/agentcfg`.
The service checks on every change and, unless `agentcfgApply` is `off`, applies the spec in
`~/.agents`, or the one `agentcfgSpecPath` names, whenever the configuration drifts.

## Install

From a clone of the fork:

```sh
CGO_ENABLED=0 go build -trimpath -o ~/.local/bin/agentcfg ./cmd/agentcfg
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
alone, which would make `apply` remove what that machine adds. A server in a role or host
must leave Codex out of its `agents`: `~/.codex/config.toml` is synced, so the machines that
declare it would add it and the others remove it again, and the spec is rejected.

```toml
[mcp_servers.search]
url = "https://search.example.com/mcp"
headers = { "X-API-Key" = "${SEARCH_API_KEY}" }

[mcp_servers.local-files]
command = "mcp-files"
args = ["--root", "~/notes"]
agents = ["claude"]      # default: every supported agent (antigravity, claude, codex)

[hooks]
claude = ["bash ~/.claude/hooks/notify.sh"]   # allowed hook commands; omitted agent = none

[plugins]
claude = ["formatter@official"]               # allowed Claude Code plugins
antigravity = ["notify"]                      # allowed Antigravity plugins, by directory

[roles.build]
mcp_servers.registry = { url = "https://registry.example.ts.net/mcp", agents = ["claude"] }

[hosts.gpu-box]
roles = ["build"]
age_recipient = "age1..."                     # this machine may decrypt secrets.age
hooks = { claude = ["python3 ~/.claude/hooks/gpu-guard.py"] }

[sync.exclude]                                # files that stay on each machine
agents = ["/skills/local-only"]               # Syncthing ignore patterns, per synced folder

[antigravity.gateway]                         # where Antigravity sends model requests
url = "https://llm-gateway.example.ts.net"
api_key = "${GATEWAY_API_KEY}"
wire_protocol = "genai"                       # default; "openai" breaks agy acp
models = ["gemini-4-neon", "claude-opus-5-5"] # the first is the default
```

`[antigravity.gateway]`, like `[sync]`, applies to the whole fleet and has no role or host
form. It sends the model requests of agy and its ACP server to an
[enterprise gateway](https://docs.cloud.google.com/gemini/enterprise/docs/ai-developer-tools/connect-enterprise-gateway)
instead of Google. `api_key` must be exactly `${VAR}`. Without `models`, agy lists the models
the gateway offers, in the gateway's order, and picks the first; name them to fix the list and
the default.

A credential in the spec must be a reference such as `${NAME}`; the spec is rejected
otherwise. For Codex, a header must be exactly `${VAR}` (written as `env_http_headers`) or
`Bearer ${VAR}` on `Authorization` (written as `bearer_token_env_var`), and an environment
variable can only pass through under its own name.

A hook is named by its command followed by its arguments, as reported by `check`. Plugin
hooks keep `${CLAUDE_PLUGIN_ROOT}` unexpanded, so the same name works on every machine.

## Synced folders

`SyncFolders` lists the folders of agent configuration that the machines of a fleet keep
identical, with their Syncthing ignore patterns; the Syncthing fork shares them on its own.

| Folder | Path | Synced |
|---|---|---|
| `agents` | `~/.agents` | Everything (skills, the spec, `secrets.age`), except `.git`, `node_modules` and Python caches. |
| `claude` | `~/.claude` | `CLAUDE.md`, `settings.json`, `keybindings.json`, `agents/`, `commands/`, `output-styles/`, `rules/` and `themes/`. |
| `codex` | `~/.codex` | `AGENTS.md`, `config.toml` and `rules/`. |
| `gemini` | `~/.gemini` | What Antigravity loads globally: `GEMINI.md`, and `skills/`, `hooks.json`, `global_workflows/`, `agents/` and `plugins/` in `config/`. Not `config/mcp_config.json`, which holds the credentials `apply` writes, nor `config/config.json` and `config/projects/`, which hold per-machine settings, nor the `antigravity*` directories. |

Everything else in an agent's own directory, such as credentials, sessions, history, caches
and databases, stays on each machine, including whatever a later version of the agent adds
there. `[sync.exclude]` keeps more out of every machine; its patterns come first, so they win.
They can only exclude: a pattern that starts with `!`, or a `#include` directive, is rejected.

## What `check` reports

| Rule | Severity | Meaning |
|---|---|---|
| `missing-server` | error | A declared server is not configured or not enabled. |
| `server-mismatch` | error | A declared server points at a different URL or command, or its headers, environment or arguments differ from the spec. |
| `undeclared-server` | error | An enabled server is not in the spec, in any scope, including one a plugin brings. |
| `undeclared-hook` | error | A hook is not in the spec, such as one reinstalled by another tool or carried by a plugin. |
| `undeclared-plugin` | error | A Claude Code plugin is enabled machine-wide but not in the spec. |
| `unexpanded-variable` | error | A Claude Code settings `env` value, or an Antigravity MCP header or variable, references `${VAR}`, which the agent passes on as written. |
| `literal-secret` | error | A credential header, token or variable is written in the file instead of referenced. For Antigravity, a credential other than the value `apply` writes from the spec. |
| `gateway-mismatch` | error | The Antigravity gateway environment file is missing, differs from `[antigravity.gateway]`, still holds a gateway the spec no longer declares, or has a mode other than 0600. |
| `shadowed-server` | warning | The same server is defined in several scopes. |
| `unset-variable` | warning | A referenced variable is not set where the check ran, so the server may start unauthenticated, or, for Antigravity, `apply` cannot write it or the gateway's API key. |

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
| `~/.gemini/config/mcp_config.json` | `mcpServers`, with every `${VAR}` replaced by its value (see below). Disabled servers the spec does not declare are kept. |
| `~/.gemini/config/hooks.json` | Hooks not in the spec are removed; a hook with `"enabled": false` is kept. |
| `~/.config/environment.d/agentcfg-antigravity.conf` | The whole file: the `AGY_LLM_GATEWAY_*` variables of `[antigravity.gateway]`, or none once the spec drops it. |

A declared server enabled in `~/.mcp.json` is corrected there, and one enabled with the same
target in managed settings is left there; neither is added to `~/.claude.json`, though an entry
already there is corrected too, since the user scope also applies outside the home directory. Within a
server, only the keys the spec describes are written (URL, command, arguments, headers,
environment), so options such as a Codex `startup_timeout_sec`, and comments in its table, are
kept. An undeclared Codex server with `enabled = false` is left alone, as `check` does. Hooks are only removed, never added, because
the spec lists the hooks allowed, not where they go. Plugins and managed files are reported but
never changed.

Antigravity does not expand `${VAR}` in `mcp_config.json`: it would send `Bearer ${TOKEN}` as
written. So `apply` writes the values, taken from the environment it runs in, which for the
Syncthing service is the systemd user environment that `agentcfg secrets sync` fills. A
declared server with a variable that is not set is left as it is, and `check` warns. The file
is not synced, so the values stay on each machine; give it mode 0600 if it already exists, as
`apply` keeps the mode of the files it rewrites.

agy reads its gateway only from environment variables, or from
`/etc/antigravity/admin_settings.json`, which needs root. So `apply` writes
`AGY_LLM_GATEWAY_URL`, `_API_KEY` (the value of the variable `api_key` names, from the
environment it runs in), `_WIRE_PROTOCOL` and `_MODELS` to an unsynced environment file with mode 0600, and leaves it
as it is while that variable is not set. The systemd user manager reads the file at login and
on `systemctl --user daemon-reload`, so user units started afterwards get it. Programs started
from the desktop, such as an editor or T3 Code running `agy acp`, inherit their launcher's
environment instead, which has the file only when the session takes its environment from the
user manager, as sessions started through systemd (for example with uwsm) do at login. Shells,
including ones opened before then or over SSH, read it with:

```sh
set -a; . ~/.config/environment.d/agentcfg-antigravity.conf; set +a
```

Writes are atomic (temporary file and rename). A symlink is kept: the file it points to is
written, with its mode. Before each run's first write the previous content is copied to
`$XDG_STATE_HOME/agentcfg/backups/<time>/`, and the last 20 runs are kept. If an agent rewrites
a file while `apply` runs, `apply` plans again from the new content instead of overwriting it.
Running `apply` twice changes nothing the second time.

## Secrets

Synced files hold only references (`${VAR}` for Claude, `env_http_headers` and
`bearer_token_env_var` for Codex). Antigravity, which cannot reference a value, gets it
written into its unsynced `mcp_config.json` by `apply`. The values live in `secrets.age` next to the spec,
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

`github.com/syncthing/syncthing/internal/agentcfg`, which only the fork can import:

```go
report, err := agentcfg.Check(ctx, agentcfg.Options{SpecPath: path})
plan, err := agentcfg.Plan(ctx, opts)    // what Apply would change
result, err := agentcfg.Apply(ctx, opts)
paths, err := agentcfg.WatchPaths(opts)  // files whose changes can change the result
folders, err := agentcfg.SyncFolders(opts) // folders to sync and their ignore patterns
```

## Where it looks

| Agent | Files |
|---|---|
| Claude Code | `~/.claude/settings.json`, `~/.claude.json` (user and local MCP scopes only), `~/.mcp.json`, `/etc/claude-code/managed-settings.json`, `managed-settings.d/`, `managed-mcp.json`, and for each plugin enabled in those settings: `plugins/installed_plugins.json`, its `hooks/hooks.json`, `.mcp.json` and the hooks or servers its `plugin.json` adds. Honors `CLAUDE_CONFIG_DIR`. |
| Codex CLI | `~/.codex/config.toml` (ignoring the `hooks.state` trust store), `~/.codex/hooks.json`, `/etc/codex/config.toml`. Honors `CODEX_HOME`. |
| Antigravity | `~/.gemini/config/mcp_config.json`, `~/.gemini/config/hooks.json`, `~/.config/environment.d/agentcfg-antigravity.conf`, and for each plugin in `~/.gemini/config/plugins/` (a directory with a `plugin.json`): its `mcp_config.json`, whose servers are named `plugin:<plugin>:<server>`, and its `hooks.json`. Not workspace `.agents/` directories, nor the legacy per-product `mcp_config.json` files. |

## Development

```sh
go test ./internal/agentcfg/... ./cmd/agentcfg/ ./lib/agentcfg/
```

The fixtures in `internal/agentcfg/testdata` are homes in known states: `clean` matches the spec with every
credential referenced, `drifted` reproduces common drift (hooks reinstalled by another tool, an
extra MCP server in a local scope, credentials written in the files, a changed URL), `plugin`
enables plugins that carry a hook and a server, and `fleet` is a machine with its own servers,
hooks and permissions on top of a shared base. All values in them are fake.

## License

MPL-2.0, as the rest of Syncthing. Until it moved into the fork, agentcfg was MIT-licensed
in its own repository, since deleted.
