# Plan: agentcfg built into the Syncthing fork, torturado/syncthing-agents

Status, 2026-11-20:
- Phase A is implemented in agentcfg (PR torturado/agentcfg#1, in the deleted repository).
- Phase B is implemented in the fork (PR torturado/syncthing-agents#7).
- The A gates are done on the six machines: credentials are in `secrets.age` and the Coolify
  token is rotated.
- The fixes found before enabling automatic apply are in agentcfg `v0.1.0` (PRs
  torturado/agentcfg#2 and #3), which the fork requires since torturado/syncthing-agents#8.
- Phase C is rolled out on the six machines, prod since 2026-11-23 (see C3). Antigravity
  support and automatic apply are on the six since 2026-11-24.

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
  `tag:syncthing` tag on every node and a policy grant (torturado/syncthing-agents#7). It was replaced
  (torturado/syncthing-agents#9) because tagging every machine in the Tailscale admin was too much
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
its `main` at `bf31b816b` (`v2.1.8-dev.16`, `CGO_ENABLED=0 go run build.go -no-upgrade build
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
  - prod, on 2026-11-27 if the others have no incidents. laptop, which was off, moved to
    the current build with the agent folders below.
- **Fleet view, 2026-11-21:** desktop, gpu-box, server-a and server-b run the fork's `main` at
  `8fae8140e` (`v2.1.8-dev.17`, torturado/syncthing-agents#9), which shares the reports through
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
  `0eb91c840` (`v2.1.8-dev.18`, torturado/syncthing-agents#10) with the `agentcfg` CLI `v0.2.0`.
  - The hand-made folders `claude-config`, `codex-config` and `codex-skills` are removed from
    Syncthing on the four; their files stay. Each device marks the others, laptop included,
    with `agentcfgSync`, and the fork shares `agentcfg-agents`, `agentcfg-claude` and
    `agentcfg-codex`. desktop created them; the other three joined desktop's offer.
  - The exclusions of the old `~/.agents/.stignore` live in `[sync.exclude]` in the spec. The
    old `.stignore-shared` files are removed. `~/.claude/mcp.json` is no longer synced.
  - Checks passed:
    - every device has the three folders at 100 % with no pull errors and no conflict copies of
      configuration;
    - a file created in `~/.claude/commands` on desktop reaches the other three in 13 s, a file
      outside the shared list stays on desktop, and the removal reaches them;
    - desktop's fleet view shows the four reports through `agentcfg-agents`, 0 errors.
  - On joining, each device's own fleet report already existed on the others, so 12 conflict
    copies of reports appeared once; they were removed. A new device has no report on the
    others yet, so it does not hit this.
  - laptop joined the same day, when it came online: the same build and CLI, its old
    folders removed, the others marked, the `[env]` line for `agentcfg.conf` added to its mise
    configuration, and its Syncthing theme regenerated, as was desktop's. Both themes show the
    agentcfg row and panel. desktop's fleet view shows the five reports, 0 errors, and `~/.agents`
    holds the same 1050 files on desktop and laptop.
  - Pending: prod, on stock Syncthing, keeps its old folders and no longer receives agent
    configuration until it moves to the fork on 2026-11-27 (mark the five, remove the old
    folders).
- **One repository, 2026-11-21:** agentcfg moved into the fork with its history:
  `internal/agentcfg` and `cmd/agentcfg`, with these documents in `docs/agentcfg`. The fork was
  its only user, and every change to the spec had to land in both repositories in order.
  torturado/agentcfg was archived, and deleted on 2026-11-22; its `v0.2.0` was the last
  release there. The CLI is built from the fork,
  `CGO_ENABLED=0 go build -trimpath -o agentcfg ./cmd/agentcfg`, and
  CI runs its checks.
- **Name, 2026-11-22:** the fork is `torturado/syncthing-agents`, renamed from
  `torturado/syncthing` so that the name says what it adds; GitHub redirects the old URLs.
  Its `README.md` describes the fork instead of upstream Syncthing. The host names of the
  machines were removed from the history, so commit hashes from before that day changed;
  the ones in these documents are the new ones.
- **prod, 2026-11-23:** moved ahead of 2026-11-27, at the user's request; the others had no
  incidents.
  - The old folders `claude-config`, `codex-config` and `codex-skills` are removed from its
    Syncthing; their files stay.
  - It runs the same binary as the others (`v2.1.8-dev.18.g1bd322e9`) at
    `/usr/local/bin/syncthing-agentcfg`, with the same drop-in, and the `agentcfg` CLI
    `v0.2.0`. The packaged binary stays installed, and the state from before is kept in
    `/root/syncthing-state-before-fork-2026-11-23.tar.gz`.
  - prod marks the five others with `agentcfgSync`, and desktop, gpu-box, server-a and server-b
    mark prod. laptop, which was off, still has to mark it.
  - Checks passed:
    - the three folders are at 100 % with no pull errors;
    - `CLAUDE.md`, `AGENTS.md` and the spec match desktop's;
    - desktop's fleet view shows the six reports, 0 errors and 0 findings;
    - a file created and then removed in `~/.agents` on desktop reaches prod in 54 s, and the
      removal in 71 s.
  - prod held old copies of the others' reports, so 5 conflict copies of reports appeared once;
    they were removed.
  - `skills/registro-obsidian` is excluded in `[sync.exclude]`: it points into `/root/obsidian`,
    which only server-a has, so it stays there and is removed elsewhere.
- **Antigravity, 2026-11-24:** desktop, gpu-box, server-a, server-b and prod run the fork's
  `main` at `715d8b237` (`v2.1.8-dev.37`, torturado/syncthing-agents#16), with the `agentcfg`
  CLI from the same commit. The binaries from before are kept next to the new ones as
  `syncthing-agentcfg.prev` and `agentcfg.prev`.
  - desktop created `agentcfg-gemini` (`~/.gemini`) and offered it to the five others; the other
    four joined the offer. Antigravity is installed only on desktop, so check skips it elsewhere.
  - `agentcfgApply` is off on every machine. On desktop, `agentcfg apply --write` added `coolify`
    and `keenable` to `~/.gemini/config/mcp_config.json`, which is not synced, and the file was
    set to 0600.
  - Checks passed:
    - the four folders are at 100 % on the five, with no new conflict copies;
    - `agentcfg check` reports 0 errors and 0 warnings on the five.
  - Pending: laptop, which was off, still runs `v2.1.8-dev.18`, which does not know the folder.
    It needs the new build before it joins.
- **Apply by default, 2026-11-24:** the same five run the fork's `main` at `43893cfe5`
  (`v2.1.8-dev.39`, torturado/syncthing-agents#18). Automatic apply is now on unless
  `agentcfgApply` is `off`, and it takes the spec in `~/.agents` without `agentcfgSpecPath`; none
  of the five sets either option. `syncthing-agentcfg.prev` and `agentcfg.prev` now hold
  `v2.1.8-dev.37`.
  - Before the restart, a dry-run `agentcfg apply` changed nothing on any of the five.
  - Checks passed:
    - the five report the spec in `agentcfg-agents`, 0 errors, 0 warnings and no apply error;
    - `keenable` removed by hand from desktop's `~/.gemini/config/mcp_config.json` was written
      back within 3 s, with the same content and mode 0600.
  - Pending: laptop, which needs this build as well.
- **Start-up fix and laptop, 2026-11-24:** the build of #18 crashed laptop at start (nil pointer
  in `lib/agentcfg`, fixed in torturado/syncthing-agents#20): index events from the first scan of
  `agentcfg-gemini` arrived before the first run had found the spec. The six machines run the
  fork's `main` at `127ed39c0` (`v2.1.8-dev.41`); `.prev` holds the build each one ran before.
  - laptop now marks prod with `agentcfgSync`, so every device marks the five others.
  - Checks passed:
    - every service restarted once, with no restart or panic since;
    - desktop's fleet view shows the six reports, 0 errors, 0 warnings and no apply error;
    - the four folders are at 100 % on the five peers of desktop, with no new conflict copies.
- **Antigravity gateway, 2026-11-27:** desktop, gpu-box, server-a, server-b and prod run the
  branch of torturado/syncthing-agents#22 at `fa3cbcd9f` (`v2.1.8-dev.43`), with the `agentcfg`
  CLI from the same commit; `.prev` holds `v2.1.8-dev.41`. Once the five ran it, the spec
  declared `[antigravity.gateway]`: CLIProxyAPI, the proxy Codex and Claude Code already use,
  with the key from `CLIPROXY_API_KEY` and twelve Gemini, Claude and GPT models, Gemini 4 Neon
  first. After review, the five moved to `915c038f9` (`v2.1.8-dev.46`), which also keeps the
  file at mode 0600; `.prev` still holds `v2.1.8-dev.41`.
  - The fleet's model proxy serves the Gemini models of its Antigravity login only with an
    effort suffix (`gemini-4-argon-high`), while agy asks for the base name and sends the effort
    apart. The proxy's `oauth.model-alias.antigravity` now also exposes `gemini-3.8-flash`,
    `gemini-4-argon`, `gemini-4-neon`, `gemini-4-carbon` and `gemini-4-hydrogen`.
  - Before each restart, a dry-run `agentcfg apply` with the spec of the time changed nothing
    but the `editor-bim` drift below.
  - Checks passed:
    - the five services restarted once with no panic, and report 0 errors and 0 warnings
      with the gateway in the spec;
    - desktop's service wrote `~/.config/environment.d/agentcfg-antigravity.conf` (mode 0600)
      1 s after the spec changed; the other four, without Antigravity, write nothing;
    - on `v2.1.8-dev.46`, the five report 0 errors and 0 warnings, and desktop's service set the
      file back to 0600 within 15 s of a `chmod 644`;
    - after `systemctl --user daemon-reload` the user manager has the three variables, and in
      a new shell `agy -p` ran a shell command with the default model and `claude-opus-5-5`;
      the proxy's log shows the Gemini requests served by its Antigravity login;
    - `agentcfg apply --write` with a copy of the spec plus the gateway wrote
      `~/.config/environment.d/agentcfg-antigravity.conf` with mode 0600, and the service,
      which still uses the spec without it, emptied the file within 6 s;
    - with only that file's variables, `agy -p` ran a shell command through the proxy with the
      default model (`gemini-4-neon`), `gemini-4-hydrogen`, `claude-fable-5-2` and
      `gpt-6.2-sol`, and `agy acp` listed the twelve models of the spec and ran one with the
      default;
    - with the `genai` protocol and no `models`, Claude models fail on their first tool call
      (`tool_use.id: Field required` from the proxy), so the spec names the models.
  - Seen during the rollout, and not caused by it: the host layer's `editor-bim` server goes
    into `~/.codex/config.toml`, which is synced, so desktop adds it and the other machines
    remove it again about every 25 s.
  - Programs that were already running, such as T3 Code with `agy acp` open, keep talking to
    Google until they restart; desktop's `.bashrc` reads the file so that new shells follow
    a change before the next login.
  - Pending: laptop, which was off. Until it runs this build, its `v2.1.8-dev.41` rejects the
    spec (`unknown keys [antigravity.gateway ...]`) and neither checks nor applies.
- **Gateway off and Codex host servers, 2026-11-27:** the spec no longer declares
  `[antigravity.gateway]` or sends `editor-bim` to Codex.
  - The gateway is off because generating with Antigravity accounts from another client is
    what Google warns can get an account flagged (pingdotgg/t3code#43026). The proxy no longer
    holds those accounts or the Gemini aliases, and agy uses each machine's own Google sign-in.
    Desktop's service emptied the environment file, and after `systemctl --user daemon-reload`
    the user manager has no `AGY_LLM_GATEWAY_*` variable; `agy -p` answered through Google.
  - `editor-bim` now lists only `claude` and `antigravity`. With laptop back online, desktop
    added it to `~/.codex/config.toml` and the other machines removed it about every 15 s. Once the spec changed, the file stayed unchanged for 100 s and no machine has the
    server. `Load` now rejects a role or host server that reaches Codex or
    replaces a top-level server Codex has.

## Rules for keeping the fork cheap

- agentcfg code lives in `internal/agentcfg` (the library), `cmd/agentcfg` (the command) and
  `lib/agentcfg` (the service), all new directories. Elsewhere, only registration: one line in
  `lib/syncthing`, routes in `lib/api`, options in `lib/config`, and one GUI template.
- No changes to BEP or the database schema.
- CI is the fork's own `.github/workflows/ci.yaml`: upstream's build and test matrix,
  golangci-lint, govulncheck and the meta checks. Upstream's other workflows (packaging,
  signing, publishing, Docker images, mirrors, nightlies, translations, PR labels) need the
  Syncthing project's secrets and infrastructure, so they are removed. When upstream changes
  one, keep it removed; when upstream changes the Go versions or these jobs in
  `build-syncthing.yaml`, carry the change over to `ci.yaml`.
- `README.md` is the fork's own. When upstream changes theirs, keep ours and carry over
  anything that still applies, such as a new build requirement.
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
