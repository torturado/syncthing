# Plan: agentcfg built into torturado/syncthing

Status, 2026-11-20:
- Phase A is implemented in agentcfg (PR torturado/agentcfg#1).
- Phase B is implemented in the fork (PR torturado/syncthing#7).
- The A gates are done on the six machines: credentials are in `secrets.age` and the Coolify
  token is rotated.
- The fixes found before enabling automatic apply are in agentcfg `v0.1.0` (PRs
  torturado/agentcfg#2 and #3), which the fork requires since torturado/syncthing#8.
- Phase C is rolled out on five machines; prod is pending (see C3).

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
happened to gpu-box).
*Gate:* a gpu-box-like fixture keeps its own permissions while sharing the base MCP servers.

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

**B5. Fleet view.** Each device shares its report as `.agentcfg/reports/<device ID>.json` in
the folder that holds the spec, rewritten when its result changes, and the GUI shows the reports
that arrived there. It needs no Tailscale setup and works without Tailscale.
- **Who reads:** the devices of that folder, which already read the spec. A receive-encrypted
  device cannot.
- **Remote apply is out of scope.** A shared report is only read and shown.
- The first version served reports on a tailnet-only listener, authorized with `WhoIs`, a
  `tag:syncthing` tag on every node and a policy grant (torturado/syncthing#7). It was replaced
  (torturado/syncthing#9) because tagging every machine in the Tailscale admin was too much
  setup.
- `docs/tailscale/agentcfg-threat-model.md` in the fork covers both the checks and the fleet view.

**B6. Notification.** A GUI notice when a device leaves the desired state.

## Phase C: rollout

**C1. Builds.** Fork binaries for linux amd64 (all six machines today), plus arm64 and darwin
if needed. On the Arch machines, keep the pacman package installed and point the user unit at
the fork binary through a systemd drop-in, so rolling back means removing the drop-in.

**C2. Order.** desktop → laptop → gpu-box → server-a → server-b → prod. prod (production) goes
last, and only after a week without incidents on the others.

**C3. Rollout, 2026-11-20.** desktop, laptop, gpu-box, server-a and server-b run the fork built from
its `main` at `1c4268385` (`v2.1.8-dev.16`, `CGO_ENABLED=0 go run build.go -no-upgrade build
syncthing`, so a static binary with modernc SQLite, as in the Debian package). The schema has not
changed since 2.1.5, so the packaged binary can take the database back.
- **Binary:** `~/.local/bin/syncthing-agentcfg` for the user units (desktop, laptop, gpu-box),
  `/usr/local/bin/syncthing-agentcfg` for `syncthing@root` (server-a, server-b, prod).
- **Drop-in:** `agentcfg-fork.conf` in `~/.config/systemd/user/syncthing.service.d/` or
  `/etc/systemd/system/syncthing@root.service.d/`:
  ```ini
  [Service]
  ExecStart=
  ExecStart=<binary> serve --no-browser --no-restart
  # root units only: a system unit does not load environment.d
  EnvironmentFile=-/root/.config/environment.d/agentcfg.conf
  ```
  Rolling back is removing the file, `daemon-reload` and restarting the unit.
- **Options:** `agentcfgEnabled = true`, with `agentcfgApply` off and the fleet view off.
- **CLI:** `agentcfg` `v0.1.0` at the same path as before.
- **Checks passed:**
  - every machine reports 0 errors and 0 warnings, and connects to all five peers;
  - a file created and then deleted on desktop reaches stock-Syncthing prod in 20 s;
  - `herdr integration install claude` on laptop shows as an `undeclared-hook` error in
    laptop's report within 3 s, `agentcfg apply` lists the removal, and uninstalling clears
    the error within 3 s.
- **Pending:**
  - prod, on 2026-11-27 if the others have no incidents;
  - laptop: it was off, so it still runs the first fork build; install the current one and
    regenerate its Syncthing theme.
- **Fleet view, 2026-11-21:** desktop, gpu-box, server-a and server-b run the fork's `main` at
  `58f004e13` (`v2.1.8-dev.17`, torturado/syncthing#9), which shares the reports through
  `~/.agents`.
  - desktop's fleet view shows all four reports, and nothing listens on port 22100;
  - a server added to gpu-box's `~/.claude.json` shows in desktop's fleet view in 3 s and clears in
    3 s once removed;
  - desktop's Syncthing theme, a third-party one, keeps its own copy of
    the GUI's `index.html`, taken when the theme is applied. A copy from before the fork hides the
    agentcfg row and panel; applying the theme again, or running the theme's
    `syncthing-theme.sh generate`, takes a new copy.
  - prod, on stock Syncthing, receives the report files with the rest of the folder.
  - The `github.com/torturado/cap/agentcfg` grant is removed from the tailnet policy. The tags
    the first version needed stay (`tag:syncthing` on the six nodes, `tag:fleet-admin` on desktop):
    a tagged node returns to a user only by logging in again, and the fork's Tailscale
    integration uses `tag:syncthing`.
- **Agent folders, 2026-11-21:** desktop, gpu-box, server-a and server-b run the fork's `main` at
  `1bd322e99` (`v2.1.8-dev.18`, torturado/syncthing#10) with the `agentcfg` CLI `v0.2.0`.
  - The hand-made folders `claude-config`, `codex-config` and `codex-skills` are removed from
    Syncthing on the four; their files stay. Each device marks the others, laptop included,
    with `agentcfgSync`, and the fork shares `agentcfg-agents`, `agentcfg-claude` and
    `agentcfg-codex`. desktop created them; the other three joined desktop's offer.
  - The exclusions of the old `~/.agents/.stignore` live in `[sync.exclude]` in the spec. The
    old `.stignore-shared` files stay on disk, unused. `~/.claude/mcp.json` is no longer synced.
  - Checks passed:
    - every device has the three folders at 100 % with no pull errors and no conflict copies of
      configuration;
    - a file created in `~/.claude/commands` on desktop reaches the other three in 13 s, a file
      outside the shared list stays on desktop, and the removal reaches them;
    - desktop's fleet view shows the four reports through `agentcfg-agents`, 0 errors.
  - On joining, each device's own fleet report already existed on the others, so 12 conflict
    copies of reports appeared once; they were removed. A new device has no report on the
    others yet, so it does not hit this.
  - Pending: prod, on stock Syncthing, keeps its old folders and no longer receives agent
    configuration until it moves to the fork on 2026-11-27 (mark the five, remove the old
    folders); laptop joins when it comes online with the current build (remove its old
    folders, mark the others).
- **One repository, 2026-11-21:** agentcfg moved into the fork with its history:
  `internal/agentcfg` and `cmd/agentcfg`, with these documents in `docs/agentcfg`. The fork was
  its only user, and every change to the spec had to land in both repositories in order.
  torturado/agentcfg is archived; its `v0.2.0` is the last release there. The CLI is built from
  the fork, `CGO_ENABLED=0 go build -trimpath -o agentcfg ./cmd/agentcfg`, and
  `.github/workflows/agentcfg.yaml` runs its checks.

## Rules for keeping the fork cheap

- agentcfg code lives in `internal/agentcfg` (the library), `cmd/agentcfg` (the command) and
  `lib/agentcfg` (the service), all new directories. Elsewhere, only registration: one line in
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
| Drift injection: `herdr integration install claude` on laptop | shows in desktop's fleet view within 1 min; with `agentcfgApply = auto`, corrected within 1 min |
| Fleet transport | reports arrive through the spec folder with no Tailscale tag or grant; nothing listens on port 22100 |
| Fleet | all six machines report zero errors |

## Open questions

- Which shared folder holds `agentcfg.toml` and `secrets.age`. Proposal: `~/.agents` (folder
  `codex-skills`), which already reaches all six machines.
- Whether `apply = auto` should ever be the default, or always stay opt-in per device.
