# agentcfg in Syncthing: threat model addendum

This addendum covers the agentcfg integration: a service that checks the
configuration of the AI coding agents on a device against a spec, can correct
it, and can show the reports of the devices that share the spec's folder. It
uses no part of the Tailscale integration; it sits next to
[threat-model.md](threat-model.md) because both are fork additions with the
same conventions. Every later change must keep the behaviour described here.
Each scenario ends with the behaviour that the code must enforce and the test
that proves it.

## Scope

All options are off by default.

| Option | Feature |
|---|---|
| `agentcfgEnabled` | Check the agents' configuration in the home directory of the user that runs Syncthing against a spec, on triggers and every `agentcfgIntervalS` seconds. Show the report in the GUI and REST API. |
| `agentcfgSpecPath` | The spec. Empty means `agentcfg.toml` at the root of the one shared folder that holds it. |
| `agentcfgApply` | `auto` writes the spec whenever the configuration drifts; `off` only reports. |
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
   `~/.claude/settings.json`, `~/.codex/config.toml` and `~/.codex/hooks.json`. An MCP server entry names a command that the agent
   runs, so writing these files is equivalent to running code as that user.
2. The report: MCP server names, URLs and commands, hook commands, plugin
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
  - `agentcfgApply` is `auto` **and** `agentcfgSpecPath` names the spec
    explicitly. A spec that was found by looking at the shared folders is
    checked, but never applied automatically, because a newly shared folder
    could otherwise become the spec without anyone choosing it.
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
  root, so a symlink that another device synced into the folder cannot lead
  a write, removal or read outside it.
- **A5. Peer reports are only read.** `GET /rest/agentcfg/fleet` takes no
  parameter. It reads the `.json` files of that directory whose name is a
  device ID, spelled as Syncthing prints it, other than ours. Each must be a
  single JSON value of at most 1 MiB, and is shown as text. Conflict copies
  and other names are ignored. A device whose spec folder is receive-only
  still reads the others' reports. Nothing in a peer report is acted on.

## Scenarios

### F1. Who can read a report

| Case | Safe behaviour | Enforced by |
|---|---|---|
| F1a. A device that shares the spec's folder. | It receives every shared report, as it receives the spec. | Trust assumptions. |
| F1b. A receive-encrypted device of that folder. | It stores the reports encrypted and cannot read them. | Syncthing's untrusted-device encryption. |
| F1c. A device that does not share the folder, a tailnet node or a LAN host. | It gets nothing: reports travel only as files of the folder, and nothing listens for them. |
| F1e. A device of the folder syncs `.agentcfg` as a symlink to somewhere else. | Writing, removing and reading the reports fail with an error instead of following it. | A4. | A4, A5. |
| F1d. A device leaves the folder. | It keeps the copies it already received; it gets no new ones. Treat a report as readable by every device that ever shared the folder. | Trust assumptions. |

### F2. Outages

| Case | Safe behaviour |
|---|---|
| F2a. A device is off or cannot connect. | Its last shared report stays, with the time of the first check that gave that result. The fleet view does not say whether the device is reachable; the device list does. |
| F2b. tailscaled is stopped. | Nothing changes: the fleet view does not use Tailscale. |

### F3. Writes

| Case | Safe behaviour | Enforced by |
|---|---|---|
| F3a. A device that shares the spec's folder adds an MCP server with a command. | Devices with automatic apply and an explicit spec path write it, and their agents may then run the command. This is the delegated trust of the spec folder. Make the folder receive-only on devices that should not author the spec; those devices then cannot share their report (A4), so the fleet view trades against this hardening. | Trust assumptions. |
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
  asset 2). A credential that matches none of the patterns, such as a bare
  argument, reaches the report. Credentials belong in references (`${VAR}`),
  which agentcfg requires of the spec and reports as `literal-secret` in the
  agents' MCP and settings files; a hook command has no such check.
- **Resource use.** One small file per device in the spec's folder, rewritten
  when its result changes. The fleet view reads the directory on request.

## Success criteria and how they are tested

| Criterion | Scenario | Test |
|---|---|---|
| A device shares its report in the spec's folder, rewrites it only when the result changes, and removes it when the fleet view is turned off, leaving peer reports alone. | A4 | `lib/agentcfg`: `TestFleetSharesReportsThroughTheSpecFolder`. |
| Peer reports are read from that directory; malformed ones show an error, conflict copies and other names are ignored. | A5, F4b | `lib/agentcfg`: `TestFleetSharesReportsThroughTheSpecFolder`. |
| Nothing is shared from a receive-only folder, and the fleet view says why while still showing the others. | A4, A5 | `lib/agentcfg`: `TestFleetDoesNotShareFromAReceiveOnlyFolder`. |
| A symlinked reports directory does not lead outside the folder. | A4, F1e | `lib/agentcfg`: `TestFleetStaysInsideTheFolder`. |
| A report withdrawn while the spec is missing comes back with it. | A4 | `lib/agentcfg`: `TestFleetSharesReportsThroughTheSpecFolder`. |
| Our own writes do not trigger another run, other tools' writes do. | A3, F3c | `lib/agentcfg`: automatic apply in a temporary home. |
| A discovered spec is never applied automatically. | A1, F3b | `lib/agentcfg`: automatic apply without `agentcfgSpecPath`. |
| The REST endpoints need the API key or a session, and CSRF protection. | Trust assumptions | They are registered on the same router as every other `/rest` endpoint. |

The end-to-end check runs on the real fleet: drift injected on one device shows
in another device's fleet view within a minute.

## Sources

- agentcfg `README.md`: what `check` reports and what `apply` writes.
