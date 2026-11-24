# agentcfg in Syncthing: threat model addendum

This addendum covers the agentcfg integration: a service that shares the
folders of the AI coding agents' configuration with the devices marked for it,
checks that configuration against a spec, can correct it, and can show the
reports of the devices that share the spec's folder. It
uses no part of the Tailscale integration; it sits next to
[threat-model.md](threat-model.md) because both are fork additions with the
same conventions. Every later change must keep the behaviour described here.
Each scenario ends with the behaviour that the code must enforce and the test
that proves it.

## Scope

`agentcfgEnabled` and `agentcfgFleetEnabled` are on by default, and
`agentcfgSync` is off on every device. On their own, the two options read,
report, and write this device's report into a folder that already holds the
spec. Nothing is shared with another device until the owner marks that device.

| Option | Feature |
|---|---|
| `agentcfgEnabled` | Check the agents' configuration in the home directory of the user that runs Syncthing against a spec, on triggers and every `agentcfgIntervalS` seconds. Show the report in the GUI and REST API. Keep the folders of agent configuration for the devices marked `agentcfgSync`. |
| `agentcfgSync` (per device) | Share the folders of agent configuration with that device (A6). The GUI shows it as "Sync AI Agent Configuration" on the device, and offers it when a device shares those folders with this one. |
| `agentcfgSpecPath` | The spec. Empty means `agentcfg.toml` at the root of the one shared folder that holds it. |
| `agentcfgApply` | Empty, the default, or `auto` writes the spec whenever the configuration drifts; `off` only reports. |
| `agentcfgFleetEnabled` | Share this device's report with the other devices of the folder that holds the spec, as `.agentcfg/reports/<device ID>.json`, and show the reports they shared in the GUI. Requires `agentcfgEnabled`. |

Out of scope:

- **Remote apply.** A shared report is only read and shown. Remote writes would
  need their own design and their own review.
- **The correctness of agentcfg itself**, such as which keys `apply` owns. That
  is specified in the agentcfg repository.
- **BEP and the database.** The integration changes neither.

## Assets

1. The agents' configuration files in the Syncthing user's home:
   `~/.claude.json`, `~/.mcp.json` (or the file it links to),
   `~/.claude/settings.json`, `~/.codex/config.toml`, `~/.codex/hooks.json`,
   `~/.gemini/config/mcp_config.json` and `~/.gemini/config/hooks.json`. An MCP server entry names a command that the agent
   runs, so writing these files is equivalent to running code as that user.
   Antigravity's `mcp_config.json` also holds credentials in clear, because
   Antigravity does not expand `${VAR}`: `apply` writes the values from its
   environment. That file is never synced (A6), and keeps its mode.
2. The folders of agent configuration: `~/.agents`, with the skills, the
   spec, `secrets.age` and the reports, and the configuration entries of
   `~/.claude`, `~/.codex` and `~/.gemini` (A6). Skills, hooks and MCP servers in them run
   as that user, so writing these folders is also equivalent to running code.
3. The report: MCP server names, URLs and commands, hook commands, plugin
   names, file paths in the home directory, variable names and the host name.
   agentcfg never reads a header or variable value into it: it records only
   whether a credential is written in a file, which variables it references
   and a digest for comparison with the spec. URLs and command lines are
   reported redacted: user information, query values, the value after a
   credential name (`Authorization: …`, `api_key=…`, `--token …`) and Bearer
   or Basic tokens become `REDACTED`. The redaction recognises these patterns
   only; a credential passed as a bare argument, such as `tool s3cr3t`, is
   reported as written. With the fleet view on, the report is also a file in
   the spec's folder, so it rests on every device of that folder.

## Trust assumptions

- **A device marked `agentcfgSync` is trusted like the user.** It receives the
  agents' configuration, and what it writes there runs on every device that
  syncs it: a hook in `settings.json`, an MCP command in `config.toml` or a
  skill. Mark only your own devices.
- **The devices of the spec's folder are trusted with its content.** They
  already receive the spec and the encrypted `secrets.age`; a
  receive-encrypted device stores only ciphertext.
- **The spec has the authority of every device that can write to its folder.**
  Syncthing does not record who changed a file. Any device that shares the
  spec's folder in send mode can change the desired state on every device that
  reads it.
- **The GUI and REST API are as trusted as before.** Whoever holds the API key
  or a GUI session can already change folders and devices, which is more than
  agentcfg lets them do.

## Design rules this addendum depends on

- **A1. Writes are local decisions.** Files are written only by `apply`, and
  only when one of these holds:
  - The local owner asks for it through `POST /rest/agentcfg/apply`.
  - `agentcfgApply` is not `off` (it is empty by default) **and** the spec is
    the one `agentcfgSpecPath` names, or the one at the root of
    `agentcfg-agents`, the folder at `~/.agents` that the service keeps itself
    for the devices marked `agentcfgSync` (A6). A spec found in any other
    shared folder is checked, but never applied automatically, because a newly
    shared folder could otherwise become the spec without anyone choosing it.
- **A2. The spec is found without guessing.** When `agentcfgSpecPath` is empty,
  the spec is `agentcfg.toml` at the root of a configured folder of the basic
  filesystem type that is not receive-encrypted. If no folder or more than one
  folder holds one, nothing is checked and the report says why.
- **A3. No write loops.** After a run, the service records the size and
  modification time of every file it watches, then checks. A file-watch
  trigger runs again only if one of those files changed since. Our own writes
  therefore never trigger another run. Writes by another tool do, and `apply`
  is idempotent, so two tools that disagree alternate at the pace of the other
  tool, not in a tight loop.
- **A4. A device shares only its own report, where the spec is.** With the
  fleet view on, the service writes its status to
  `.agentcfg/reports/<device ID>.json` in the folder that holds the spec, named
  by its own device ID, and nowhere else. The file is replaced through a
  temporary name that Syncthing does not scan, and rewritten only when what the
  status says changes, so the folder changes once per change of result rather
  than on every check. Turning the fleet view or agentcfg off removes the file.
  Nothing is written when the spec is not in a shared folder, or when that
  folder is receive-only or receive-encrypted on this device; the fleet view
  says why. Every report file is reached through `os.Root` at the folder's
  root, and the reports directory must not be a symlink, so a symlink that
  another device synced into the folder cannot redirect a write or read.
- **A5. Peer reports are only read.** `GET /rest/agentcfg/fleet` takes no
  parameter. It reads the `.json` files of that directory whose name is a
  device ID, spelled as Syncthing prints it, other than ours. Each must be a
  single JSON value of at most 1 MiB, and is shown as text. Conflict copies
  and other names are ignored. A device whose spec folder is receive-only
  still reads the others' reports. Nothing in a peer report is acted on.

- **A6. The folders of agent configuration are shared only by the owner's
  choice, and only configuration.** The service keeps one folder per directory
  that agentcfg's `SyncFolders` lists: `~/.agents`, `~/.claude`,
  `~/.codex` and `~/.gemini`, or `CLAUDE_CONFIG_DIR` and `CODEX_HOME`. Each has the ID
  `agentcfg-<name>` on every device, so they meet without anyone choosing an ID
  or a path. A folder is shared with exactly the devices marked
  `agentcfgSync`, and with none while no device is marked. In that case the
  folder is removed from the configuration and its files are left in place. A
  device syncs a folder only when both ends have marked each other, as for any
  Syncthing folder; an offer from a device that this one has not marked is only
  shown. An untrusted device cannot be marked. Each folder's `.stignore` is
  written by the service before the folder first starts:
  - the spec's `[sync.exclude]` patterns for that folder;
  - for `~/.claude`, `~/.codex` and `~/.gemini`, the configuration entries and then `/*`,
    so credentials, sessions, history, databases and anything that a later
    version of an agent adds stay on the device;
  - for `~/.agents`, repositories, dependencies and caches.

  agentcfg rejects a spec exclusion that includes files (`!`) or is a directive
  or comment. The file is rewritten, and the folder rescanned, when the
  patterns change. A directory that another folder already syncs is not shared
  a second time; the status says which folder to remove. When a marked device
  already offers a folder that this device creates, this device is joining, and
  the files that it would sync are dated to the epoch first. Syncthing then
  keeps the others' copy of every file that differs, and this device's copy
  becomes a conflict copy.

## Scenarios

### F1. Who can read a report

| Case | Safe behaviour | Enforced by |
|---|---|---|
| F1a. A device that shares the spec's folder. | It receives every shared report, as it receives the spec. | Trust assumptions. |
| F1b. A receive-encrypted device of that folder. | It stores the reports encrypted and cannot read them. | Syncthing's untrusted-device encryption. |
| F1c. A device that does not share the folder, a tailnet node or a LAN host. | It gets nothing: reports travel only as files of the folder, and nothing listens for them. | A4, A5. |
| F1d. A device leaves the folder. | It keeps the copies it already received; it gets no new ones. Treat a report as readable by every device that ever shared the folder. | Trust assumptions. |
| F1e. A device of the folder syncs `.agentcfg` or `.agentcfg/reports` as a symlink. | Writing and reading the reports fail with an error instead of following it, even to a place inside the folder, where Syncthing would not sync the report. **Residual:** the check and the operation are separate calls, so a symlink swapped in between can redirect them to a file of the same name elsewhere in the folder, never outside it. | A4. |

### F2. Outages

| Case | Safe behaviour |
|---|---|
| F2a. A device is off or cannot connect. | Its last shared report stays, with the time of the first check that gave that result. The fleet view does not say whether the device is reachable; the device list does. |
| F2b. tailscaled is stopped. | Nothing changes: the fleet view does not use Tailscale. |

### F3. Writes

| Case | Safe behaviour | Enforced by |
|---|---|---|
| F3a. A device that shares the spec's folder adds an MCP server with a command. | Devices with automatic apply write it, and their agents may then run the command. This is the delegated trust of the spec folder. Make the folder receive-only on devices that should not author the spec; those devices then cannot share their report (A4), so the fleet view trades against this hardening. | Trust assumptions. |
| F3b. A tailnet device auto-shares a new folder with `agentcfg.toml` at its root. | It is never applied automatically. With no other spec, it is checked and reported. With another spec already present, nothing is checked until one of them is removed, and the report says so. | A1, A2. |
| F3c. Another tool rewrites a file agentcfg owns. | With automatic apply, agentcfg corrects it after the debounce. Two tools that disagree alternate at the other tool's pace. | A3. |
| F3d. Another device wants this device to apply. | There is no way to ask: shared reports are only read. | A5. |
| F3e. Another device writes a file named after our device ID. | It stays until our result changes, when we replace it, or until we turn the fleet view off, when we remove it. Until then the fleet view of other devices shows it. This is the delegated trust of the folder, as F4a. | A4, trust assumptions. |

### F4. Reading the reports

| Case | Safe behaviour |
|---|---|
| F4a. A device of the folder shares a forged report, its own or another device's. | Syncthing does not record who wrote a file, so a forged report shows as written. Any device that can write to the folder can already change the spec, which is worse. The file is size-limited and parsed as JSON, the GUI shows it as text, and nothing in it changes our configuration. |
| F4b. A report file is malformed or too large. | That device's entry shows an error; the others are shown. |
| F4c. A request tries to make the fleet view read another path. | The endpoint takes no parameter, so it reads only the reports directory of the spec's folder. |

### F5. Other threats

- **Disclosure through the report.** URLs and command lines from the spec and
  the agents' files are reported with recognised credentials redacted (see
  asset 3). A credential that matches none of the patterns, such as a bare
  argument, reaches the report. Credentials belong in references (`${VAR}`),
  which agentcfg requires of the spec and reports as `literal-secret` in the
  agents' MCP and settings files; a hook command has no such check. The
  credentials `apply` writes for Antigravity are compared by digest and never
  reported.
- **Resource use.** One small file per device in the spec's folder, rewritten
  when its result changes. The fleet view reads the directory on request.
  The folders of agent configuration are small, because only configuration is
  synced.

### F6. Sharing the folders of agent configuration

| Case | Safe behaviour | Enforced by |
|---|---|---|
| F6a. A device that this one has not marked shares the folders with it. | Nothing syncs. The GUI shows one notice for that device, instead of three new-folder prompts with the wrong default path. Syncing starts only when the owner marks the device. | A6. |
| F6b. An untrusted device is marked. | The configuration drops the mark, and nothing is shared with it. | A6, `DeviceConfiguration.prepare`. |
| F6c. The spec's exclusions try to sync `.credentials.json` with `!`, or include another file. | agentcfg rejects the spec. Patterns that are already in place stay, the folders keep syncing, so the fix can arrive, and the status shows the error. | A6. |
| F6d. An agent update adds a new state or credential file to `~/.claude`, `~/.codex` or `~/.gemini`. | It is not synced: only the listed configuration entries are. | A6. |
| F6h. `apply` writes credentials into `~/.gemini/config/mcp_config.json`. | The file is not among the synced entries, so the values stay on the machine that wrote them. | A6, `antigravity.Shared`. |
| F6e. A device joins with a newer, default `settings.json`. | The other devices keep theirs. The joining device's copy becomes a conflict copy, which stays on that device because the root patterns ignore it. A conflict copy inside a synced directory, such as `agents/`, syncs like any other file. If the joining device is marked before the others offer the folders, Syncthing's usual rule applies, and the newer copy wins while the other becomes a conflict copy. | A6. |
| F6f. `~/.claude` is already a folder with another ID. | It is not added a second time, so two folders never scan one directory. The status names the folder to remove. | A6. |
| F6g. The last marked device is unmarked. | The folders are removed from the configuration, and the files stay on every device. | A6. |
| F6h. A marked device adds a hook to `settings.json`. | It syncs, and it runs on every device: this is the trust given by marking the device. `agentcfg check` reports any hook that the spec does not allow. | Trust assumptions. |

## Success criteria and how they are tested

| Criterion | Scenario | Test |
|---|---|---|
| A device shares its report in the spec's folder, rewrites it only when the result changes, and removes it when the fleet view is turned off, leaving peer reports alone. | A4 | `lib/agentcfg`: `TestFleetSharesReportsThroughTheSpecFolder`. |
| Peer reports are read from that directory; malformed ones show an error, conflict copies and other names are ignored. | A5, F4b | `lib/agentcfg`: `TestFleetSharesReportsThroughTheSpecFolder`. |
| Nothing is shared from a receive-only folder, and the fleet view says why while still showing the others. | A4, A5 | `lib/agentcfg`: `TestFleetDoesNotShareFromAReceiveOnlyFolder`. |
| A symlinked reports directory does not lead outside the folder. | A4, F1e | `lib/agentcfg`: `TestFleetStaysInsideTheFolder`. |
| A report withdrawn while the spec is missing comes back with it. | A4 | `lib/agentcfg`: `TestFleetSharesReportsThroughTheSpecFolder`. |
| Our own writes do not trigger another run, other tools' writes do. | A3, F3c | `lib/agentcfg`: automatic apply in a temporary home. |
| A spec found outside `~/.agents` is never applied automatically; the one in `~/.agents` is, by default. | A1, F3b | `lib/agentcfg`: automatic apply without `agentcfgSpecPath`. |
| The REST endpoints need the API key or a session, and CSRF protection. | Trust assumptions | They are registered on the same router as every other `/rest` endpoint. |
| The folders are kept for exactly the marked devices, with only the agents' configuration and the spec's exclusions first, and removed once no device is marked. The files stay. | A6, F6d, F6g | `lib/agentcfg`: `TestAgentFoldersAreSharedWithTheMarkedDevices`. |
| A directory that another folder already syncs is not shared twice. | A6, F6f | `lib/agentcfg`: `TestAgentFolderTakenByAnotherFolderIsReported`. |
| A joining device yields its differing files, and only the files it would sync. | A6, F6e | `lib/agentcfg`: `TestJoiningDeviceYieldsToTheOthers`. |
| An exclusion that includes files, or a directive, is rejected. | F6c | agentcfg: `TestLoadRejectsInvalidSpecs`, `TestSyncFoldersShareConfigurationOnlyAndExcludeWhatTheSpecSays`. |

The end-to-end check runs on the real fleet: drift injected on one device shows
in another device's fleet view within a minute.

Before the real fleet, two instances on loopback, each with its own home,
check the folders end to end. The second device clicks the notice and joins.
It receives the first device's settings even though its own are newer, and
keeps its own as conflict copies. Credentials, history and `auth.json` stay
on each device, and a spec exclusion holds. An edit propagates back, and
unmarking leaves the files in place.

## Sources

- agentcfg `README.md`: what `check` reports and what `apply` writes.
