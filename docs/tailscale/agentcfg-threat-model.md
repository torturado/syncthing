# agentcfg in Syncthing: threat model addendum

This addendum covers the agentcfg integration: a service that checks the
configuration of the AI coding agents on a device against a spec, can correct
it, and can show the reports of the whole tailnet. It extends
[threat-model.md](threat-model.md) and relies on its trust assumptions and
rules (R1 to R9). Every later change must keep the behaviour described here.
Each scenario ends with the behaviour that the code must enforce and the test
that proves it.

## Scope

All options are off by default.

| Option | Feature |
|---|---|
| `agentcfgEnabled` | Check the agents' configuration in the home directory of the user that runs Syncthing against a spec, on triggers and every `agentcfgIntervalS` seconds. Show the report in the GUI and REST API. |
| `agentcfgSpecPath` | The spec. Empty means `agentcfg.toml` at the root of the one shared folder that holds it. |
| `agentcfgApply` | `auto` writes the spec whenever the configuration drifts; `off` only reports. |
| `agentcfgFleetEnabled` | Serve this device's report on TCP port 22100 of its tailnet addresses to nodes that the tailnet policy grants `github.com/torturado/cap/agentcfg`, and show the reports of the tagged peers in the GUI. Requires `agentcfgEnabled`. |

Out of scope:

- **Remote apply.** Nothing on the tailnet listener writes. Remote writes would
  need their own access level and their own review.
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
   reported as written.

## Trust assumptions

- Everything in [threat-model.md](threat-model.md#trust-assumptions) holds:
  the local tailscaled and its LocalAPI are trusted, tailnet source addresses
  can be trusted because of return-path routing, and our tailscaled's view is
  the source of truth.
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
- **A4. The tailnet listener is tailnet-only.** It is bound to each of our
  tailnet addresses on port 22100, never to a wildcard or LAN address, and it
  follows the addresses as they change. While the tailnet is unavailable
  (S4 of the main model) nothing listens, and open connections are closed.
- **A5. Every request is authorized on its own.** Before anything else, the
  listener asks LocalAPI `WhoIs` about the remote address. The request is
  served only if all of these hold:
  - the tailnet is available;
  - the remote address is not one of ours;
  - the node is in our tailnet's domain, its key has not expired and it
    carries `tag:syncthing`;
  - the node holds `github.com/torturado/cap/agentcfg` with a value
    `{"access": "read"}`.

  Anything else, including a `WhoIs` error, a malformed value or another
  access level, is answered with 403. The Syncthing API key and GUI sessions
  play no part, in either direction.
- **A6. The listener only reads.** It serves `GET /agentcfg/report` and nothing
  else. Requests are bounded by header and read timeouts and a header size
  limit.
- **A7. The aggregator talks only to tagged peers.** `GET /rest/agentcfg/fleet`
  takes no address. It asks only the tagged peers in our tailnet table, on port
  22100 of their tailnet addresses, without proxies or redirects, with a
  timeout per peer, and reads at most 1 MiB of JSON from each. Peer reports
  are shown as text.

The capability name is in a namespace the fork owns; `syncthing.net/cap/...`
belongs to upstream. A grant names the aggregator as source and the reporting
nodes as destination:

```json
{
  "grants": [
    {"src": ["tag:fleet-admin"], "dst": ["tag:syncthing"], "ip": ["tcp:22100"],
     "app": {"github.com/torturado/cap/agentcfg": [{"access": "read"}]}}
  ]
}
```

The aggregator must itself be tagged `tag:syncthing` (A5), so in practice it
carries both tags.

## Scenarios

### F1. Who can read a report

| Case | Safe behaviour | Enforced by |
|---|---|---|
| F1a. A node of our tailnet without the grant. | 403. | A5. |
| F1b. A node shared in from another tailnet, even if a grant there names it. | 403: its MagicDNS name is in the other tailnet's domain. | A5, as S1a. |
| F1c. A node with the grant but without `tag:syncthing`, such as a phone. | 403. A grant to `*` is not enough on its own. | A5. |
| F1d. A node whose key expired. | 403. | A5, as S2b. |
| F1e. Another user on the same host, connecting from our tailnet address. | 403. Nothing listens on loopback. | A5, A4. |
| F1f. A LAN host, or a host spoofing a tailnet address. | Nothing listens on the LAN. A spoofed handshake cannot complete (return-path routing), and `WhoIs` would not name a node. | A4, A5. |
| F1g. The grant is removed. | The next request gets 403. There is no session to revoke. | A5. |
| F1h. An address is reused by a new node. | `WhoIs` names the current owner, so each request is judged against the node that holds the address when it arrives. **Residual:** a response that was authorized just before the address moved may be routed to the new owner. It holds one read-only report. | A5. |

### F2. tailscaled stops or is unhealthy

| Case | Safe behaviour |
|---|---|
| F2a. tailscaled is stopped, the LocalAPI is gone, the backend is not `Running`, or it uses userspace networking. | The tailnet becomes unavailable (S4a to S4c). The listeners close within a few seconds, with their open connections, and come back when the tailnet does. A request that arrives in between gets 403, because the tailnet is unavailable or `WhoIs` fails. BEP, discovery and the GUI are unaffected: the listener is a separate service. |
| F2b. Our tailnet addresses change. | The listeners move to the new addresses. |
| F2c. A `WhoIs` call hangs. | It times out after five seconds, and the request gets 403. |

### F3. Writes

| Case | Safe behaviour | Enforced by |
|---|---|---|
| F3a. A device that shares the spec's folder adds an MCP server with a command. | Devices with automatic apply and an explicit spec path write it, and their agents may then run the command. This is the delegated trust of the spec folder. Make the folder receive-only on devices that should not author the spec. | Trust assumptions. |
| F3b. A tailnet device auto-shares a new folder with `agentcfg.toml` at its root. | It is never applied automatically. With no other spec, it is checked and reported. With another spec already present, nothing is checked until one of them is removed, and the report says so. | A1, A2. |
| F3c. Another tool rewrites a file agentcfg owns. | With automatic apply, agentcfg corrects it after the debounce. Two tools that disagree alternate at the other tool's pace. | A3. |
| F3d. A tailnet node asks to apply. | There is no endpoint for it. | A6. |

### F4. The aggregator

| Case | Safe behaviour |
|---|---|
| F4a. A compromised peer answers with a forged report. | It can misreport only its own state, which it controls anyway. The response is size-limited and parsed as JSON. The GUI shows it as text. Nothing in it changes our configuration. |
| F4b. A peer does not answer, or answers slowly. | That peer's entry shows an error after the per-peer timeout. The others are not delayed beyond it. |
| F4c. A request tries to make the aggregator fetch another address. | The endpoint takes no address, so it cannot be used to reach anything but the tagged peers' port 22100. |
| F4d. The aggregator is compromised. | It can read the reports of every node that grants it, and nothing else. |

### F5. Other threats

- **Disclosure through the report.** URLs and command lines from the spec and
  the agents' files are reported with recognised credentials redacted (see
  asset 2). A credential that matches none of the patterns, such as a bare
  argument, reaches the report. Credentials belong in references (`${VAR}`),
  which agentcfg requires of the spec and reports as `literal-secret` in the
  agents' MCP and settings files; a hook command has no such check.
- **Tailnet admin compromise.** An attacker who controls the policy can grant
  read access to any tagged node, and so read every report, with the limits on
  what a report holds described above. Write access cannot be granted, because nothing on the
  listener writes.
- **Resource use.** One `WhoIs` per request, with a timeout. The policy decides
  which nodes can reach port 22100 at all; grant `tcp:22100` only to the
  aggregator.

## Success criteria and how they are tested

Unit tests use a fake LocalAPI in `lib/tailscale` and a fake tailnet in
`lib/agentcfg`.

| Criterion | Scenario | Test |
|---|---|---|
| A node with the grant reads the report. | A5 | `lib/agentcfg`: granted node gets 200 and the report. |
| A node without the capability, or with another access level, gets 403. | F1a, F1g | `lib/agentcfg`: no capability and `{"access": "write"}` get 403. |
| A node of another tailnet, an untagged node, our own address and an unknown address are refused. | F1b, F1c, F1e, F1f | `lib/tailscale`: `TestCapabilities`. |
| With tailscaled unavailable the listener is closed. | F2a | `lib/agentcfg`: the listener stops accepting connections when the tailnet reports no addresses, and opens again when they return. `lib/tailscale`: `Capabilities` refuses while unavailable. |
| Our own writes do not trigger another run, other tools' writes do. | A3, F3c | `lib/agentcfg`: automatic apply in a temporary home. |
| A discovered spec is never applied automatically. | A1, F3b | `lib/agentcfg`: automatic apply without `agentcfgSpecPath`. |
| The REST endpoints need the API key or a session, and CSRF protection. | Trust assumptions | They are registered on the same router as every other `/rest` endpoint. |

The end-to-end checks of the plan run on the real fleet: drift injected on one
device shows in the aggregator's fleet view within a minute, a node without
the capability gets 403 from port 22100, and with tailscaled stopped nothing
listens on port 22100 and syncing goes on.

## Sources

- [threat-model.md](threat-model.md): R2, R3 and S4, which this addendum
  follows for request authorization and outages, and the sources listed there
  for `WhoIs`, application capabilities and sharing.
- [golink](https://github.com/tailscale/golink/blob/d4c752ba2a71793dd8035a5b23746f2c97831c89/golink.go)
  and
  [tsidp](https://github.com/tailscale/tsidp/blob/3c4364fa73369fc641652272b55f67c7fb73fbca/server/appcap.go):
  `WhoIs` on every request, deny by default, capabilities in the service's own
  domain.
- agentcfg `README.md`: what `check` reports and what `apply` writes.
