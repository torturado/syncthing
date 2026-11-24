# syncthing-agents

An unofficial fork of [Syncthing][syncthing] that keeps the configuration of AI
coding agents (Claude Code, Codex CLI and Antigravity) the same on every machine you own, and
that uses Tailscale to find and authorize those machines.

It is Syncthing with additions. The sync engine, the protocol, the
configuration and the web GUI are upstream's, and a device running this fork
syncs with devices running stock Syncthing. Each addition is described below
with how it is turned on.

This fork is not affiliated with or endorsed by the Syncthing Foundation.
Report problems with it here, not on Syncthing's forum or issue tracker.

## What it adds

### Agent configuration that syncs itself

Mark a device with **Sync AI Agent Configuration** (*Edit device*, on both
ends), and the fork shares four folders with it, with the same IDs and paths
on every device:

| Folder | Path | What syncs |
|---|---|---|
| `agentcfg-agents` | `~/.agents` | everything: skills, the spec, `secrets.age` |
| `agentcfg-claude` | `~/.claude` | `CLAUDE.md`, `settings.json`, `keybindings.json`, `agents/`, `commands/`, `output-styles/`, `rules/`, `themes/` |
| `agentcfg-codex` | `~/.codex` | `AGENTS.md`, `config.toml`, `rules/` |
| `agentcfg-gemini` | `~/.gemini` | `GEMINI.md`, and in `config/`: `skills/`, `hooks.json`, `global_workflows/`, `agents/`, `plugins/` |

Credentials, sessions, history and caches stay on each machine. No folder is
set up by hand: the fork writes the ignore patterns, and a device that joins
an existing fleet takes the fleet's copy of each file and keeps its own as a
conflict copy. More files can be kept on each machine with `[sync.exclude]`
in the spec.

### Checks against a spec

Put an `agentcfg.toml` spec in `~/.agents` and every device checks its agents
against it: which MCP servers are configured, which hooks and plugins are
installed, and whether a credential is written into a file instead of
referenced. Antigravity cannot reference a credential, so `apply` writes the
value into its `mcp_config.json`, which stays on each machine. The result shows in the GUI, and the **fleet view** shows every
device's result, shared through the same folder. Each device also corrects the
drift itself, unless `agentcfgApply` is `off`. It does so only with the spec in
`~/.agents` or the one `agentcfgSpecPath` names.

Credentials travel encrypted with [age][age] in `secrets.age`, and each machine
decrypts them into a local environment file.

The same checks run outside Syncthing with the `agentcfg` command. See
[docs/agentcfg](docs/agentcfg/README.md).

### Tailscale

All three are off by default:

- **Discovery** (`tailscaleDiscoveryEnabled`): finds the other Syncthing nodes
  on the tailnet at their tailnet addresses.
- **Authorization by the tailnet policy** (`tailscaleAutoAcceptDevices`): a
  grant in the tailnet policy decides which new devices join and which folders
  they get.
- **Listening on the tailnet only** (`tailscaleListenOnly`).

The device ID stays the identity, and every connection is authenticated with
TLS as in upstream. See [docs/tailscale](docs/tailscale/README.md) and its
[threat model](docs/tailscale/threat-model.md).

## Status

A personal project, run daily on a handful of Linux machines. There are no
releases and no binaries; build it from source. It follows upstream `main`,
and CI runs upstream's tests and the fork's on Linux, macOS and Windows.

## Build

Requires Go (see `go.mod`).

```sh
go run build.go -no-upgrade build syncthing     # ./syncthing
CGO_ENABLED=0 go build -trimpath -o agentcfg ./cmd/agentcfg  # ./agentcfg
```

`-no-upgrade` stops the binary from replacing itself with an upstream release.
Run it in place of `syncthing`, with the same configuration directory; the new
options appear under *Actions → Advanced → Options*.

## Documentation

- [docs/agentcfg/README.md](docs/agentcfg/README.md): the spec, `check`,
  `apply`, secrets and the synced folders.
- [docs/agentcfg/integration-plan.md](docs/agentcfg/integration-plan.md): how
  agentcfg runs inside Syncthing, and its rollout.
- [docs/tailscale/README.md](docs/tailscale/README.md): the Tailscale options
  and the policy they need.
- Threat models: [Tailscale](docs/tailscale/threat-model.md) and
  [agentcfg](docs/tailscale/agentcfg-threat-model.md).
- [docs/tailscale/fork-maintenance.md](docs/tailscale/fork-maintenance.md):
  what carrying the fork on top of upstream costs.
- Everything else is Syncthing's: [documentation][docs], [goals][goals],
  [getting started][start].

## Code layout

The additions live in their own directories, so merging upstream stays cheap:
`internal/agentcfg` (the library), `cmd/agentcfg` (the command),
`lib/agentcfg` (the service inside Syncthing) and `lib/tailscale`. Elsewhere
the changes are small hooks into upstream code. The Go module path stays
`github.com/syncthing/syncthing` for the same reason.

## Security

Report a vulnerability in the fork's additions privately, through *Security →
Report a vulnerability* on this repository. One that also affects stock
Syncthing goes to security@syncthing.net, as upstream asks.

## License

[MPL-2.0](LICENSE), as Syncthing.

[syncthing]: https://syncthing.net/
[age]: https://age-encryption.org/
[docs]: https://docs.syncthing.net/
[goals]: GOALS.md
[start]: https://docs.syncthing.net/intro/getting-started.html
