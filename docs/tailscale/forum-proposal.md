# Forum proposal: optional Tailscale integration

This is the text proposed for the Development category of
forum.syncthing.net. It is written to be posted as is. The numbers come from the
implementation in this fork, measured against upstream `main` at f2ed52347. The
fork maintenance figures come from [fork-maintenance.md](fork-maintenance.md).

---

**Title:** Proposal: optional Tailscale integration (discovery,
tailnet-authorized devices, listening on the tailnet only)

Hi all,

Many of us run Syncthing between machines that are already on a tailnet. Today
that usually means:

- setting a tailnet address or MagicDNS name, such as `tcp://100.x.y.z:22000`,
  on each device, because discovery does not reliably find the tailnet route
  ([forum 15293](https://forum.syncthing.net/t/syncthing-tailscale/15293),
  [forum 24553](https://forum.syncthing.net/t/tailscale-between-two-synology-nas/24553));
- accepting each new device ID by hand, or through an introducer, even though
  the tailnet already knows which machines belong to us.

I'd like to propose a small, opt-in integration with the local tailscaled. I
have built it, and tested it end to end with the stock tailscaled and Headscale
as the control server. Before preparing upstream PRs, I'd like to hear whether
you would consider it, and in what shape.

## What it is not

- **Not a new transport.** I read #9998 (DERP) and #10877 (iroh). Syncthing
  still dials plain TCP. The tailnet only provides routable addresses and
  answers "who is at the other end of this socket?".
- **Not a replacement for device IDs.** Every connection is still
  authenticated by TLS against the device ID. For devices that the tailnet
  authorized, tailnet authorization is an extra condition, never a substitute.
- **Not embedded tsnet.** Syncthing does not become a tailnet node of its own.
  It reads from the system tailscaled over the LocalAPI socket, and needs no
  write access.

## What it does

There are three independent advanced options, all off by default.

### 1. `tailscaleDiscoveryEnabled`: a discovery `Finder`

- It follows the tailscaled IPN bus (`WatchIPNBus`). It re-reads `Status`
  shortly after notifications, coalescing bursts, and every 30 s. It does not
  parse the netmap from the bus, because recent daemons reject
  `NotifyInitialNetMap`.
- It resolves a device ID to `tcp://<tailnet-ip>:22000` for peers of our own
  tailnet that are tagged `tag:syncthing`.
- To learn which device ID a peer runs, it opens a TLS 1.3 connection without a
  client certificate to the peer's port 22000. The result counts only when the
  peer asks for a client certificate. By then the client has verified the
  peer's `CertificateVerify` and `Finished`, so the peer has proven it holds the
  key. The probe aborts at that point, so it never sends our certificate or
  device ID. The probed side does see the probe's tailnet address. Each
  successful probe costs one "Failed TLS handshake" warning on the probed
  side. Probes happen when a peer appears, when its addresses change, and
  after the tailnet watch restarts.
- Discovery results remain hints, like global and local discovery.

### 2. `tailscaleAutoAcceptDevices`: let the tailnet policy authorize new devices

calmh wrote in 2017 that "it's obviously insecure to just add whomever
connects". This option does not do that. An unknown device is added only when
LocalAPI `WhoIs` on the peer's tailnet address shows that the node:

- is named in our own tailnet's MagicDNS domain, so it is not a node from
  another tailnet;
- has a key that has not expired;
- is tagged `tag:syncthing`;
- holds the application capability `syncthing.net/cap/syncthing`, granted
  explicitly in the policy file;
- is not already bound to another device.

There are two ways to get there:

- **After a probe.** The probe has proven which device ID the peer runs, and
  `WhoIs` must name the same node that was probed. This is how two devices
  that do not know each other find and add each other.
- **When the peer connects.** The check is then on the actual socket:
  - the connection must be direct TCP, which excludes Syncthing relays and
    proxies (Tailscale may still carry the packets over DERP);
  - it must arrive on one of our tailnet addresses, and not come from one of
    ours;
  - the node must have held the address since before the connection was
    established.

This follows the pattern of Tailscale's own golink and tsidp. The capability
value lists folders:

```json
"grants": [
  {"src": ["tag:syncthing"], "dst": ["tag:syncthing"], "ip": ["tcp:22000"]},
  {"src": ["tag:syncthing"], "dst": ["tag:syncthing"],
   "app": {"syncthing.net/cap/syncthing": [{"folders": ["photos"]}]}}
]
```

Grants are directional. Each node reads the grant from the peer towards
itself, so a device never gets more than the side that admits it grants. The
example grants both directions, because both nodes are tagged
`tag:syncthing`.

**A device added this way:**

- is named after its MagicDNS name, not its Hello, and cannot be renamed by its
  Hello;
- is bound to the node's `StableNodeID`, so an address that a new owner reuses
  inherits nothing;
- is never an introducer;
- is only dialed over TCP at its node's tailnet addresses;
- is shared the granted folders that exist locally.

**The grant is an upper bound on folders.** The configuration's prepare step
removes any folder outside the grant from a bound device on every commit,
whichever mechanism tried to share it: the GUI, an introducer, or auto-accept.
Auto-accept is limited to granted folders. Without that, auto-accept would also
share an existing local folder with any peer that merely offers its ID.

The grant also drives sharing. A granted local folder is shared with the
device, and if it is unshared in the GUI it is shared again on the next
re-check while the grant still lists it. To stop sharing it, remove it from
the grant.

**Each connection is checked:**

- The node that authorizes a connection is recorded on the socket.
- That node must have held the address in our peer table since before the
  connection was established. This closes address hand-overs between the
  handshake and admission.
- It is asked again after the Hello.
- It is compared with the binding at final admission.

Established connections of bound devices are re-checked after each tailnet
update that our tailscaled reports, and on the 30 s refresh. Once the tailnet
stops vouching for the device, they are closed. That covers a deleted node, an
expired key, a removed tag or grant, and an unreachable LocalAPI. In the
end-to-end runs on Headscale, this took about 2 s.

- An unknown device that does not pass becomes a pending device, exactly as
  today.
- A bound device that does not pass is refused, and stays configured.
- Devices configured by hand are never affected.
- Turning auto-accept off stops new enrollment. Bound devices still need the
  tailnet's authorization.

### 3. `tailscaleListenOnly`: listen only on the tailnet

This option restricts inbound BEP only:

- Each TCP and QUIC listener is bound to the node's tailnet addresses.
- No relay listener, NAT port mapping or STUN runs.
- Without a tailnet address, no BEP listener runs. It never falls back to
  `0.0.0.0`.

Outbound connections, including through relays when relays are enabled, are
unchanged, and so is the GUI.

## Security

I wrote the threat model before the code:
[threat-model.md](https://github.com/torturado/syncthing/blob/main/docs/tailscale/threat-model.md).
It went through several rounds of adversarial review, which changed the
design. In short:

- **Fail closed everywhere.** A `WhoIs` failure refuses only the connection or
  enrollment it was for. When the tailnet watch or `Status` shows the whole
  integration is unavailable, the effects are wider. No device is added,
  tailnet-authorized devices are refused, the finder returns nothing, and
  listen-only has no BEP listener. That happens when:
  - tailscaled is down or logged out;
  - it uses userspace networking;
  - the LocalAPI cannot be reached.

  Devices configured by hand keep syncing over whatever other routes they
  have, such as the LAN, unless listen-only is on.
- **Trust delegation.** Enabling option 2 delegates authorization to whoever
  can edit the policy and owns `tag:syncthing`. That is why it is opt-in on each
  node.
- **A compromised node with the grant** is limited to the folders its grant
  lists. It cannot introduce further devices, take over another node's binding,
  or register a second device ID.
- **Residual risk.** If our tailscaled is cut off from the control plane, it
  keeps its last netmap, as every tailnet service does.

## Tests

**Unit tests** cover:

- the probe;
- `WhoIs` response validation and LocalAPI outage handling;
- per-socket admission;
- address continuity and reuse;
- grant ordering;
- the configuration's grant bound;
- the auto-accept allowlist;
- dial-target and listener selection.

**The end-to-end suite** is `go test -tags tailscale_e2e ./test/tailscale`. It
runs on a real tailnet:

- Headscale 0.29.7 as the control server;
- a DERP relay;
- one Docker container per node, with tailscaled 1.104.7 and kernel TUN.

It shows the following:

- Both nodes have the options on and the grant runs both ways. A fresh node
  then syncs the granted folder to 100 % with no manual step on either side.
- A tagged node without the grant stays pending and gets no folder.
- Revoking a grant, deleting a node and expiring its key each disconnect the
  device in about 2 s. The goal was under a minute.
- A node that reuses an expelled node's address, even presenting the expelled
  device's certificate, is refused.
- **LocalAPI cut** while tailscaled keeps running: bound devices disconnect,
  and an authorized newcomer stays pending.
- **tailscaled stopped:** a device configured by hand keeps syncing over the
  LAN, and no new device is accepted.
- In listen-only mode, the kernel's sockets on port 22000 are bound only to
  the tailnet addresses.

## Cost to the codebase

**Existing files.** 19 Go files are touched, with +765/−21 lines, of which 342
are tests. Not counting tests, the lines added are:

| Area | Lines added | What changes |
|---|---|---|
| `lib/connections` | 306 | one check before the Hello, a re-check loop, dial-target and listener selection |
| `lib/config` | 53 | three options, two device fields, and the grant bound in `prepare` |
| `lib/model` | 11 | the auto-accept allowlist and no Hello renaming of bound devices |
| `lib/discover` | 22 | register the finder; no cache writes when caching is disabled |
| `lib/dialer`, `lib/nat`, `lib/api`, `lib/syncthing` | 31 | small hooks |

**New code.** `lib/tailscale` and the finder add about 1,120 lines of code and
720 lines of tests. The end-to-end harness adds about 1,260 lines.

**Dependency.** `tailscale.com/client/local`, pinned at v1.98.1, because later
versions raise the `go` directive above this repository's 1.26.2. It pulls in:

- 70 `tailscale.com` packages;
- 17 more modules in `go.mod`;
- +3.0 MB of binary, from 37.8 MB to 40.8 MB (+7.8 %), built with
  `go run build.go -no-upgrade build syncthing`.

`govulncheck` reports nothing reachable, and no module-level finding that
upstream `main` does not already have. One wrinkle: tailscale v1.98.1 asks
for a `go-json-experiment/json` that does not compile with Go 1.27, so that
module is bumped.

**Fork maintenance today**
([fork-maintenance.md](https://github.com/torturado/syncthing/blob/main/docs/tailscale/fork-maintenance.md)).
I reapplied the series onto the last three releases, v2.1.5, v2.1.6 and
v2.1.7. Each one then built and passed the race
unit suite and the end-to-end suite. The code conflicts were the same on all
three:

- `lib/discover`, because the releases predate the cachedFinder refactor
  (#10956). Ten lines were resolved by hand.
- The auto-accept loop in `lib/model`, because the releases predate a4a2da477
  (#10935). Three lines were resolved by hand.
- `go.mod` and `go.sum` on v2.1.5 and v2.1.6, from dependency drift. These were
  regenerated with `go get` and `go mod tidy`.

All of the new code applied cleanly. Rebasing onto today's `main` costs nothing,
because `main` has not moved since the fork's base.

I know dependencies are a sensitive topic. These are the alternatives I see:

- **(a) Keep the dependency, behind a build tag** (`notailscale`, or opt-in
  `tailscale`).
- **(b) A small in-tree LocalAPI client.** It would be about 200 lines of stdlib
  HTTP over the socket, for `status`, `whois` and `watch-ipn-bus`. The catch is
  that the LocalAPI is not a stable public API, and the macOS GUI build and
  Windows use other transports, which the official client handles.
- **(c) Leave it out of Syncthing.** An external helper could do much of this
  through the REST API:
  - watch for `PendingDevicesChanged`, call `WhoIs`, and add devices;
  - write device addresses, which approximates option 1;
  - write listen addresses, which approximates option 3.

  It cannot do the per-connection authorization and re-checks, which need to
  sit in Syncthing's connection pipeline.

## Questions for you

1. Is an optional integration of this kind something you'd accept in
   principle? If so, with the dependency (a), as an in-tree client (b), or not
   at all (c)?
2. Is `syncthing.net/cap/syncthing` an acceptable capability name? Tailscale
   asks applications to use a domain they control.
3. Folders are listed in the tailnet policy. Would you rather that only device
   membership comes from the tailnet, and folder sharing stays a local decision
   (for example, a per-folder "share with tailnet-authorized devices" flag)?
4. Should this stay as advanced options only, or also get a GUI section later?
5. The finder assumes port 22000 on peers. Is that acceptable for a first
   version?
6. Should a node that loses contact with the control plane for longer than some
   limit fail closed? That would close the residual risk above, at the cost of
   availability during control-plane outages.

Thanks for reading. I'm happy to split this into three PRs, one per option,
in the order above.
