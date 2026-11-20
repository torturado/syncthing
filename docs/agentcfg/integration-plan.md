# Plan: agentcfg built into torturado/syncthing

Status, 2026-11-20:
- Phase A is implemented in agentcfg (PR torturado/agentcfg#1).
- Phase B is implemented in the fork (PR torturado/syncthing#7).
- Phase C and the per-machine parts of the A gates are pending: the checks on the six machines,
  moving their credentials into `secrets.age`, and rotating the Coolify token.

Deviations from this plan, kept on purpose:
- Automatic apply needs an explicit `agentcfgSpecPath`. A spec that is only found in a shared
  folder is checked, never written.
- The interval option is `agentcfgIntervalS`, after the repository's `…IntervalS` options.
- The service is always registered and idles while it is disabled, so options take effect
  without a restart.

## Decision

The fork imports agentcfg **as a Go module** and runs it as one more service inside the
Syncthing process. agentcfg stays a standalone library and CLI in its own repository, with its
own tests and releases.

Why this shape:

- Agent configuration formats change every few weeks, while Syncthing releases roughly monthly
  and its code is security-sensitive. The two must not share a release cadence or a review bar.
- Upstream will never take agentcfg, so this part of the fork is permanent. Every line of it is
  paid again on every rebase, so it must live in one package plus a handful of registration
  points.
- Anyone who does not run the fork can still use the CLI from cron or a systemd timer.

Rejected:

| Alternative | Why not |
|---|---|
| Copy agentcfg's code into the fork | Two copies to keep in step, and the fork diff grows with every agent format change. |
| Carry agentcfg data in BEP messages | Protocol change: breaks interoperability with stock Syncthing peers and is far harder to review. |
| A separate daemon that talks to Syncthing's REST API | Viable fallback, but one more thing to install and supervise on every machine. Keep it as plan B if the fork integration costs too much to rebase. |

## Preconditions from the fork

- `feat/tailscale-authorization` and `feat/tailscale-listen-only` are on the fork's `main`, so
  `lib/tailscale` (LocalAPI client, `WhoIs`, capability parsing) can be reused.
- The fork is rebased on the latest upstream release and passes `go run build.go test`.

Phase A does not depend on either and can start now.

## Phase A: agentcfg on its own

Each step lands with tests and is checked against the six real machines before the next.

**A1. Public library API.** Move the code out of `internal/` (another module cannot import it)
into a package with `Check(ctx, Options) (Report, error)`. Make `Report` JSON with a schema
version, and keep the CLI a thin wrapper. Lower the `go` directive to 1.26, because the fork's
`go.mod` says `go 1.26.2` and importing agentcfg must not raise it.
*Gate:* the fork builds with agentcfg added to `go.mod` and its `go` line unchanged.

**A2. Plugin hooks and plugins.** Read the enabled Claude Code plugins (`enabledPlugins`,
`plugins/installed_plugins.json`) and each one's `hooks/hooks.json`. Today a plugin can add a
hook that `check` never sees.
*Gate:* a fixture where an enabled plugin carries a hook produces `undeclared-hook`.

**A3. Layers in the spec.** Use `base`, then roles, then a host match (hostname or tailnet name),
merged in that order, so unifying the fleet no longer erases a machine's own settings (what
happened to aimery).
*Gate:* an aimery-like fixture keeps its own permissions while sharing the base MCP servers.

**A4. Secrets by reference.**
- Synced files carry only references: `${VAR}` for Claude, `env_http_headers` and
  `bearer_token_env_var` for Codex.
- The values live in `secrets.age`, which is synced and encrypted to each machine's age public
  key listed in the spec.
- `agentcfg secrets sync` decrypts it into a local, unsynced, mode 0600 environment file. That
  file is read by systemd user units (`environment.d`) and by login shells.
- Rotate the Coolify token as part of this step.

*Gate:* `check` reports zero `literal-secret` on all six machines, and gitleaks finds nothing in
the synced folders.

**A5. `apply`.**
- It owns only the keys it manages: `mcpServers` in `~/.claude.json`, and in Codex
  `config.toml` the `mcp_servers` tables but never `projects` or `hooks.state`.
- Writes are atomic (temp file + rename). If the target is a symlink, it writes the resolved
  file, so the link is kept.
- It backs up before writing. `--diff` is the default; `--write` is explicit.

*Gate:*
- After `apply`, `projects.*` in `~/.claude.json` and Codex `trust_level` entries are unchanged.
- A herdr reinstall in a temporary home is corrected.
- Running it twice changes nothing.

**A6. Stable output.** JSON report and documented exit codes, so the daemon and the GUI never
parse text.

## Phase B: inside the fork

**B1. Service.** Add `lib/agentcfg` as a service started by `lib/syncthing` only when
`agentcfgEnabled` is true (default false). Options in `lib/config`:
- `agentcfgEnabled`;
- `agentcfgSpecPath` (default: look for `agentcfg.toml` at the root of the shared folders);
- `agentcfgApply` (`off` | `auto`);
- `agentcfgInterval`.

**B2. Triggers.**
- Syncthing events for the folder holding the spec (`LocalIndexUpdated`, `ItemFinished`).
- A file watcher on the unsynced files that other tools rewrite (`~/.claude.json`,
  `~/.codex/hooks.json`, `~/.claude/settings.json`).
- The interval as a backstop. All of them are debounced.

**B3. API.** `GET /rest/agentcfg/report` and `POST /rest/agentcfg/apply`, behind the same API key
and CSRF checks as the rest of the REST API. An `AgentConfigChanged` event goes on the event bus.

**B4. GUI.** A status badge for this device and a findings table, built with the existing
Angular modal patterns in `gui/default`. No new frontend dependencies.

**B5. Fleet view.** The servers' GUI listens on 127.0.0.1, so it cannot be used for this.
- **Listener:** each device opens a separate, tailnet-only HTTP listener that serves only the
  report.
- **Authorization:** each request is authorized with `WhoIs` against its own capability, not
  the Syncthing API key.
- **Capability name:** the capability needs a namespace the fork owns, for example
  `github.com/torturado/cap/agentcfg` with `{"access": "read"}`. `syncthing.net/cap/...` belongs
  to upstream.
- **View:** casa aggregates the peers' reports in its GUI.
- **Remote apply is out of scope.** It needs a separate `apply` access level and its own review.
- **Before any code:** write a threat-model addendum in the same style as
  `docs/tailscale/threat-model.md`.

**B6. Notification.** A GUI notice when a device leaves the desired state.

## Phase C: rollout

**C1. Builds.** Fork binaries for linux amd64 (all six machines today), plus arm64 and darwin
if needed. On the Arch machines, keep the pacman package installed and point the user unit at
the fork binary through a systemd drop-in, so rolling back means removing the drop-in.

**C2. Order.** casa → portatil → aimery → dockerpapa → prox → docker. docker (production) goes
last, and only after a week without incidents on the others.

## Rules for keeping the fork cheap

- agentcfg code lives in `lib/agentcfg`. Elsewhere, only registration: one line in
  `lib/syncthing`, routes in `lib/api`, options in `lib/config`, and one GUI template.
- No changes to BEP or the database schema.
- Rebase on every upstream release and record the conflicts. If the agentcfg part causes
  conflicts on two consecutive releases, switch to plan B (separate daemon).

## Verification

| Check | Pass |
|---|---|
| Upstream suite on the fork, `go run build.go test` with `-race` | green |
| agentcfg suite | green, including the A2–A5 fixtures |
| Interoperability | a stock Syncthing peer keeps syncing with a fork peer |
| Drift injection: `herdr integration install claude` on portatil | shows in casa's fleet view within 1 min; with `agentcfgApply = auto`, corrected within 1 min |
| Authorization | a tailnet node without the capability gets 403 from the report listener; with `tailscaled` stopped the listener is closed and syncing is unaffected |
| Fleet | all six machines report zero errors |

## Open questions

- Which shared folder holds `agentcfg.toml` and `secrets.age`. Proposal: `~/.agents` (folder
  `codex-skills`), which already reaches all six machines.
- Whether `apply = auto` should ever be the default, or always stay opt-in per device.
