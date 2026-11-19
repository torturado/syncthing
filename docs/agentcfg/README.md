# agentcfg

Checks that the AI coding agents on a machine match a declared configuration, so every
machine in a personal fleet runs the same MCP servers, no hook appears without you knowing, and
no credential sits in a file that gets synced or committed.

It reads only the configuration that applies machine-wide and leaves agent state alone:
project trust, session history and per-repository `.mcp.json` files are never reported.

Supported agents: Claude Code and Codex CLI.

## Usage

```sh
agentcfg check                 # reads ~/.config/agentcfg/spec.toml
agentcfg check --spec path.toml
```

Exit status: `0` the machine matches the spec, `1` at least one error, `2` the spec or a
configuration file could not be read. Run it on every machine (cron, a systemd timer, or over
SSH) to detect drift.

## Spec

```toml
[mcp_servers.keenable]
url = "https://api.keenable.ai/mcp"

[mcp_servers.local-files]
command = "mcp-files"
agents = ["claude"]      # default: every supported agent

[hooks]
claude = ["bash ~/.claude/hooks/notify.sh"]   # allowed hook commands; omitted agent = none
```

## What it reports

| Rule | Severity | Meaning |
|---|---|---|
| `missing-server` | error | A declared server is not configured or not enabled. |
| `server-mismatch` | error | A declared server points at a different URL or command. |
| `undeclared-server` | error | An enabled server is not in the spec, in any scope. |
| `undeclared-hook` | error | A hook is not in the spec, such as one reinstalled by another tool. |
| `literal-secret` | error | A credential header, token or variable is written in the file instead of referenced. |
| `shadowed-server` | warning | The same server is defined in several scopes. |
| `unset-variable` | warning | A referenced variable is not set where the check ran, so the server may start unauthenticated. |

Secret values are never read into the report: a field only records whether it is written in
the file and which variables it references.

## Where it looks

| Agent | Files |
|---|---|
| Claude Code | `~/.claude/settings.json`, `~/.claude.json` (user and local MCP scopes only), `~/.mcp.json`, `/etc/claude-code/managed-settings.json`, `managed-settings.d/`, `managed-mcp.json`. Honors `CLAUDE_CONFIG_DIR`. |
| Codex CLI | `~/.codex/config.toml` (ignoring the `hooks.state` trust store), `~/.codex/hooks.json`, `/etc/codex/config.toml`. Honors `CODEX_HOME`. |
