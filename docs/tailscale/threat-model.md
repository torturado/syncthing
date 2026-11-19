# Tailscale integration: threat model

This document defines the security behaviour of the native Tailscale
integration before any of it is implemented. Every later change must keep the
behaviour described here. Each scenario ends with the behaviour that the code
must enforce and the test that proves it.

## Scope

The integration adds three independent, opt-in features. All of them are off by
default.

| Option | Feature |
|---|---|
| `tailscaleDiscoveryEnabled` | A discovery `Finder` that resolves a device ID to `tcp://<tailnet-ip>:22000` for tailnet peers tagged `tag:syncthing`, kept current from the tailscaled IPN bus. |
| `tailscaleAutoAcceptDevices` | An unknown device is added automatically, named after its tailnet node, only when LocalAPI `WhoIs` shows that the tailnet policy grants it the `syncthing.net/cap/syncthing` application capability. Otherwise it stays pending, as today. |
| `tailscaleListenOnly` | Syncthing listens only on the node's own tailnet addresses. |

Out of scope:

- **Replacing TLS identity.** The device ID stays the SHA-256 of the peer's
  certificate, and every connection is authenticated by TLS exactly as before.
  For devices that the tailnet authorized, tailnet authorization is a second,
  independent condition.
- **Running Syncthing as its own tailnet node (tsnet).** The integration uses
  the system tailscaled.
- **tailscaled in userspace-networking mode.** Inbound connections reach
  Syncthing from `127.0.0.1` in that mode, so `WhoIs` cannot identify the
  peer. The integration treats that mode as unavailable.
- **Policies that grant the capability to IP literals.** `WhoIs` evaluates
  capabilities per pair of nodes, not per address family or port. Grants must
  name identities, such as tags, users or groups.

## Assets

1. Folder contents: confidentiality and integrity.
2. The set of devices that may receive each folder: `devices` and folder
   sharing in the configuration.
3. The device's private key, and with it the device ID.

## Trust assumptions

- **Local tailscaled and its LocalAPI are trusted.** They belong to the same
  trusted computing base as the host. Whoever can replace the LocalAPI socket
  (root on Linux, or the user session on the macOS GUI build) can already read
  Syncthing's data. Other local users are *not* trusted.
- **The tailnet policy decides authorization only when the local owner opts
  in** with `tailscaleAutoAcceptDevices`. Enabling the option delegates "may
  this device join, and which folders may it get" to whoever can edit the
  tailnet policy file and own `tag:syncthing`.
- **Tailnet source addresses can be trusted because of return-path routing.**
  Replies to a tailnet address leave through the tailnet interface, and
  WireGuard delivers them only to the node that holds that address in our
  netmap. TCP and QUIC handshakes need the client to see those replies. A host
  that spoofs a tailnet source address on another interface therefore cannot
  complete a handshake. The ingress filtering in tailscaled is defence in depth
  on top of that, not the guarantee: on Linux it drops tailnet IPv4 sources
  that arrive on other interfaces.
- **Our tailscaled's view is the source of truth.** A revocation takes effect
  when our tailscaled receives it from the control plane. Partitions are a
  residual risk, described in S2a.
- **Discovery results are hints, never authentication.** This matches the
  existing global and local discovery, which anyone can poison.

## Design rules this model depends on

- **R1. Identity stays TLS.** A tailnet address or name never stands in for a
  device ID. Dialing an address that a Finder returned succeeds only when TLS
  presents the expected certificate.
- **R2. Which connections can be authorized.** A connection can be authorized
  only if all of these hold:
  - It is direct TCP. Relayed connections never are, because their endpoints
    belong to the relay. QUIC connections never are either: outbound QUIC shares
    a wildcard UDP socket, so its local address cannot be checked.
  - Its local address is one of our tailnet addresses.
  - Its remote address is not one of ours. This excludes other users on our own
    host.
  - LocalAPI `WhoIs` on the remote address reports a node whose MagicDNS name
    is in our own tailnet's domain. Nodes shared in or out are named in their
    own tailnet's domain.
  - The node is not expired.
  - The node carries `tag:syncthing`.
  - The node holds `syncthing.net/cap/syncthing` with a value that parses.

  The capability is the authorization. Network grants only decide which side
  can open the connection. Authorization does not require that the authorized
  side could have dialed.
- **R3. The `WhoIs` outcome affects only that connection.** Each outcome is
  handled separately:

  | Outcome | Effect |
  |---|---|
  | No matching node, for example a LAN address | That connection is not authorized. |
  | Capability absent | That connection is not authorized. |
  | Capability malformed | That connection is not authorized. |
  | LocalAPI error | That connection is not authorized. |

  None of these touches other connections. The integration as a whole counts as
  unavailable only when the IPN bus or `Status` fails, or when tailscaled is
  not `Running` or not on a kernel TUN. Then R7 applies.
- **R4. Tailnet-authorized devices are bound to a node, not to an IP.** An
  auto-added device stores its node's `StableNodeID` (`tailnetNodeID`) and the
  folders that its grant allows (`tailnetFolders`). A node can be bound to at
  most one device.
- **R5. Authorization is attached to the socket.** The node that the tailnet
  authorizes is recorded on the connection before the Hello exchange. After
  the Hello, the tailnet is asked again, so a revocation or outage during the
  Hello is not missed.

  When the BEP session is finally admitted, a bound device is accepted only if
  its connection was authorized for the bound node. The final check applies
  the folder list it receives before BEP starts. It waits until every
  configuration subscriber, the model included, has applied the change, so
  index updates and block requests already see the new list. A grant that shrank during the Hello
  therefore never reaches the session. This covers inbound, outbound and
  additional connections alike.

  The node that `WhoIs` names must also be the node that our peer table held
  for the remote address, **since before the connection was established**.
  Our table only ever lags tailscaled, and the replies of a TLS handshake go
  to the address's current owner. So a node that held the address before the
  handshake, and still holds it when `WhoIs` answers, is the node that
  completed the handshake. If the address changed hands, or our table learned
  of its owner only after the connection began, the connection is not
  authorized, and an unknown device stays pending.

  The configuration change that adds a device checks once more, inside the
  change, that the device is not known, not ignored and that its node is not
  bound to another device. A device the user ignored is never added.
- **R6. The grant is an upper bound.** The capability value is
  `{"folders": ["<folder ID>", ...]}`. When several grants match, their lists
  are merged. A value with no folders allows the device to join and shares
  nothing.

  The configuration enforces the bound on every commit. It removes from each
  folder any bound device whose `tailnetFolders` does not list that folder,
  whichever mechanism tried to share it: the GUI, an introducer, or
  auto-accept. A bound device auto-accepts only listed folders. Granted folders
  that exist locally are shared with it.

  On every new connection, the device's current grant is applied before the
  Hello exchange and again at admission (R5). A device that was offline while
  its grant shrank therefore never starts BEP with the old list.

  On a live connection, a folder that leaves the grant is unshared by a
  configuration commit, exactly as when a user unshares it in the GUI:
  - index updates are refused as soon as the commit is published;
  - block requests are refused once the model has applied the change, a
    moment later;
  - requests already in flight complete.

  The integration adds no data-path check of its own.
- **R7. Re-checks and fail closed.** Every established connection of a bound
  device goes through `WhoIs` again whenever the tailnet state changes, and at
  least every 30 seconds. A failed check closes the connection, and the device's
  `tailnetFolders` follow its current grant.

  While the integration is unavailable:
  - no device is auto-added;
  - bound devices are refused and disconnected;
  - the Finder returns no addresses;
  - listen-only mode has no listeners.

  Devices configured by hand, without a binding, behave exactly as without the
  integration.

  As with any revocation, a message that the device sent before the change was
  observed may still be processed. Closing does not wait for the BEP farewell,
  and nothing sent after the change is accepted.
- **R8. Limited privileges for tailnet devices.** An auto-added device is
  never an introducer, so it cannot add further devices.
- **R9. Identity probes do not disclose anything.** To map a tagged peer to a
  device ID, the Finder probes it. The probe uses a dedicated TLS 1.3 client
  configuration that carries no certificate and never resumes a session. It
  tries each of the peer's addresses on port 22000.

  A result counts only when the handshake reaches the point where the peer asks
  for our client certificate. By then the client has verified the peer's
  `CertificateVerify` and `Finished`, so the peer has proven it holds the key.
  The probe aborts there.

  The probe therefore learns a device ID whose key the peer has proven it
  holds, and never reveals ours. Results from a peer whose addresses changed
  in the meantime are discarded. Ordinary BEP dials still present our
  certificate, as they always have.

## Enrollment: how two devices that do not know each other connect

Take a node A that already has folder `photos`, and a new node B. Both are
tagged, and both enable discovery and auto-accept. The policy:
- allows TCP port 22000 in both directions, so each side can probe and dial
  the other;
- grants `{"folders": ["photos"]}` in both directions, A to B and B to A.

1. Each Finder sees the other node in the netmap and probes it (R9), so A
   learns B's device ID and B learns A's.
2. Each side calls `WhoIs` on the other's tailnet address. A device that is
   unknown, probed, and authorized (R2) is added and bound (R4).
3. One side dials the other through the Finder's address. The receiving side
   authorizes the connection with `WhoIs` (R2, R5). If it has not added the
   dialer yet, it adds it now, so either order works.
4. A shares `photos` with B because the grant lists it. B auto-accepts the
   offered `photos` because its own grant for A lists it, and creates it in the
   default folder path. Files then flow normally.

Grants are directional, and each side reads the grant from the other node
towards itself. If only B to A is granted:
- A adds B, when it can reach B to probe it, or when B connects to it;
- B leaves A pending;
- nothing is shared until the reverse grant exists.

If network access only allows one direction, say A to B, only A can probe and
dial:
- A enrolls B by probing it, using B's grant towards A;
- B can enroll A when A's connection arrives, using A's grant towards B.

In short, the side that receives a connection enrolls the other with the grant
towards itself.

Enrollment is deliberately one-sided: a device never gets more than the side
that admits it grants.

The end-to-end test covers the one-way case. The test node `node-c` has
`tag:syncthing` but not the tag that the grant names, so only the grant towards
it exists. It adds the other node and dials it, stays pending there, and gets
no folder.

## Scenarios

### S1. A shared or compromised node enters the tailnet

| Case | Safe behaviour | Enforced by |
|---|---|---|
| S1a. A foreign node: a node shared in to us, or a node of a foreign user that our share makes visible to us (a sharee node). | It is not discovered and is never authorized. Its MagicDNS name is in the other tailnet's domain, and our policy cannot give it our tags. Our own nodes that we share out stay in our domain and are treated like any other node of ours. | Domain check in the Finder and in R2. |
| S1b. A new node with `tag:syncthing` but no grant. | It can be discovered, but it is not authorized. If it connects, it shows up as a pending device, exactly as an unknown device does today, and it receives no folders. We never dial it unless its device ID is configured by hand. | R2: no capability means no enrollment. The probe reveals nothing about us (R9). |
| S1c. A node with the grant is compromised. | This is the delegated trust boundary. The node gets the folders listed in its grant and nothing more. It cannot introduce other devices (R8). It cannot obtain other folders, because the configuration enforces the grant as an upper bound (R6). It cannot impersonate another device, because TLS identity is required (R1). It cannot take over another node's binding (R4, R5), and it cannot register more than one device. Limit the damage with narrow grants, receive-only folders and versioning. | R1, R4, R5, R6, R8. |
| S1d. An authorized node presents another device's certificate, or cycles certificates. | Without the private key, the TLS handshake fails, and the probe fails too (R9). A second device ID from an already bound node is not added and stays pending. | R1, R4, R9. |
| S1e. A node uses a misleading name. | The device name comes from the tailnet: the MagicDNS name that control assigns, never the name in the remote `Hello`. | Enrollment uses `WhoIs` `Node.ComputedName`. |
| S1f. A host spoofs a tailnet source address, on the LAN or through a relay. | It is rejected. A spoofed handshake cannot complete, because replies go through WireGuard (see the trust assumptions). Relayed connections are never authorized (R2). As defence in depth, the local address must be one of ours. With `tailscaleListenOnly`, nothing listens on other interfaces at all. | R2, return-path routing, listen-only mode. |
| S1g. Another user on our own host connects from our own tailnet address. | It is not authorized. | R2 rejects remote addresses that are ours. |

### S2. A node leaves the tailnet, its key expires, or its access changes

| Case | Safe behaviour | Enforced by |
|---|---|---|
| S2a. The node is deleted or logged out (expelled). | Once our tailscaled has the new netmap, the node's connections close within seconds, and always within one minute. Future connections from it are refused. The device configuration stays in place, so the device comes back if the same node is authorized again. | IPN bus update, then re-check (R7). |
| S2b. The node key expires. | Same as S2a while the key is expired. Tagged nodes have key expiry disabled by default, so this mostly affects user nodes. | R2 rejects expired nodes (`Expired`, or `KeyExpiry` in the past). |
| S2c. The tag or the grant is removed. | Connections close within 30 seconds of our tailscaled receiving the change, and later connections are refused. | R7 periodic re-check. R2 requires both the tag and the capability. |
| S2d. The grant's folder list shrinks or grows. | Within the same bound, folders that are no longer listed are unshared. The configuration drops them on commit, and the model then refuses their indexes and block requests, as for any unshared folder. Newly listed folders that exist locally are shared. | R6, R7. |
| S2e. The local owner disables `tailscaleAutoAcceptDevices`. | No new devices are auto-added. Bound devices still require tailnet authorization (R4, R5, R7), because turning an option off must not make them unconditionally trusted. To make a bound device a normal one, clear its `tailnetNodeID`. | The binding, not the option, decides. |

**Residual risk: our tailscaled is cut off from the control plane.** Our
tailscaled then keeps its last netmap and keeps answering `WhoIs` from it. An
expelled node that keeps its WireGuard key could go on talking to us until our
tailscaled reconnects. The same holds for every other service on the tailnet;
Tailscale keeps working with stale netmaps by design.

Failing closed whenever control is unreachable would disconnect every bound
device during ordinary control outages. This design does not do that; the
choice is left open for the upstream discussion.

### S3. An IP address is reused

The tailnet can assign an address that a deleted node used to hold to a new
node.

- Nothing trusts an address on its own. The Finder rebuilds its address table
  from each netmap, probes a peer again when its address set changes, and
  discards results from probes started before the change.
- Authorization resolves the address with `WhoIs`, so it sees the current
  owner. For a bound device the `StableNodeID` must match, so a new owner of
  the old address is refused even if it holds the right certificate.
- A device that was configured by hand and dialed at a stale address still has
  to pass TLS (R1).

A device found by a probe is added only if `WhoIs` names the same node that
the peer table held when the probe ran. Connections are held to the same rule
(R5): `WhoIs` must name the node that the peer table held for the remote
address when the check began.

The continuity rule in R5 closes the race on incoming connections. Suppose our
tailscaled moves an address to a new node after accepting the old owner's
handshake. Our peer table then learns of the new owner only after that
connection was established, so the connection is refused. This matters because
a socket admitted under the wrong node could deliver buffered index entries,
including deletions and directories, which the puller applies without a block
request.

The cost of the rule is that a node which has just joined may have its first
connections refused until our peer table lists it. That takes about a second
after tailscaled reports the change, and the node's next dial, or our probe of
it, enrolls it.

### S4. tailscaled is stopped, has crashed, or is unhealthy

| Case | Safe behaviour |
|---|---|
| S4a. tailscaled is stopped or crashed, the IPN bus breaks, or the LocalAPI socket is gone while the data plane still runs. | The Finder clears its table and reports an error in the discovery status. Nothing is auto-added, and unknown devices go to pending as today. Bound devices are disconnected and refused (R7). Devices configured by hand keep working through every other address and discovery method. The watcher reconnects with backoff, and normal operation resumes when the LocalAPI is back. |
| S4b. The backend is not `Running`: it is logged out, needs login or is stopped. | Same as S4a. |
| S4c. Userspace networking (`TUN=false`). | Same as S4a. |
| S4d. `tailscaleListenOnly` with no tailnet address available. | No BEP listeners at all, and no fallback to `0.0.0.0`. Listeners come back when an address reappears. |
| S4e. A `WhoIs` error or timeout on a new connection. | That connection is not authorized (R3), and it is retried on the normal schedule. |
| S4f. A `Status` call that hangs. | It times out, the watch restarts, and S4a applies in the meantime. |

### S5. Listen-only mode

Listen-only reduces exposure. It is not an authorization control.

**What it does:**
- It replaces the effective listener set. Every TCP and QUIC listener is bound
  to our tailnet addresses, whatever host the configuration names: wildcard,
  custom or LAN.
- It drops relay listeners.
- It turns off NAT port mapping.
- It moves the listeners as our tailnet addresses change.

**What it does not do:**
- It does not restrict outgoing connections. Syncthing can still dial devices
  that are configured by hand at non-tailnet addresses.
- A handshake that was already in flight when the listeners moved may be
  aborted, for example when a QUIC listener closes, or it may complete. If it
  completes, it is handled under the normal rules, and R2 to R5 still apply
  if it belongs to a bound device.

### S6. Other threats

- **Discovery poisoning.** A tagged peer can only answer probes with a
  certificate whose key it holds (R9). The Finder then maps that device ID to
  the peer's addresses. Connections still require TLS. The worst case is a
  failed dial, which presents our certificate as any BEP dial does.
- **Stale discovery state.** Bus notifications can be lost, so they only
  prompt a `Status` read, and `Status` is also read every 30 seconds. The
  Finder is registered with both cache times zero. With both at zero, the
  discovery manager neither reads nor stores lookups (a change to
  `cachedFinder`), so it never serves or reports an entry that the Finder no
  longer holds.
- **Probe noise.** Each successful probe makes the remote Syncthing log one
  "Failed TLS handshake" warning, because the client aborts. Probes run only
  when a peer appears or its address set changes. Failed probes retry with
  exponential backoff, capped at two minutes.
- **Tailnet admin compromise.** An attacker who controls the tailnet policy can
  grant the capability to any tagged node. This is the delegated trust. It only
  affects nodes that enabled `tailscaleAutoAcceptDevices`, and only for folders
  that exist on those nodes. Document this risk next to the option.
- **Local LocalAPI impersonation.** See the trust assumptions. Out of scope.

**Kept on purpose, beyond the success criteria:**
- the tag requirement for admission, which makes a grant to `*` insufficient on
  its own;
- one device per node, which bounds what a compromised node can register;
- merged folder lists, which let policies combine grants.

## Success criteria and how they are tested

These tests run against a real tailnet. The control server is Headscale v0.29
or later, which supports `grants` with `app` capabilities. Every node runs a
real tailscaled with kernel TUN in its own Docker container. Unit tests cover
the following with a fake LocalAPI:
- the probe;
- each `WhoIs` outcome, including foreign-domain nodes;
- expired keys;
- relayed and self connections;
- binding;
- per-socket admission;
- the grant upper bound;
- the auto-accept allowlist.

Headscale has no node sharing, so foreign nodes are covered only by unit tests.
The data path itself relies on Syncthing's existing checks that index updates
and block requests name a folder shared with the device. The tests show that
the configuration cannot hold such a share.

| Criterion | Scenario | Test |
|---|---|---|
| A new device with the grant syncs 100 % with no manual steps. | Enrollment | A tagged node with the grant starts with an empty configuration plus the options. It reaches 100 % completion on the granted folder, and the seeded file arrives with the same content. |
| A device without the grant receives no folders. | S1b | A tagged node without the grant stays pending on the others. No folder is shared with it, and no file reaches it. |
| An expelled device loses its connection in under a minute. | S2a, S2b, S2c | `headscale nodes delete`, `headscale nodes expire`, and separately a policy that removes the grant. Measure the time until the surviving peer reports the device disconnected. Timing starts at the control-plane action, so it also bounds the time our tailscaled takes to observe the change. |
| An address is reused. | S3 | **Unit test, deterministic,** with a fake LocalAPI that moves one address to a new node. While the peer table still names the old node and `WhoIs` already names the new one, an unknown device that connects from the address stays pending. A connection established before the table learned of the new owner is refused. Once the table names the new node, the old binding is refused from that address, and the new owner's device enrolls on a fresh connection. **End-to-end:** Headscale is limited to a /30 and a /126 with random allocation, so the next node gets the expelled node's addresses. That node also presents the expelled device's certificate, and it is refused. |
| With tailscaled stopped, Syncthing keeps its known devices and accepts no new ones. | S4a | **LocalAPI cut, while the tailnet still carries traffic:** a proxy in front of the socket closes every LocalAPI connection, including the IPN stream and pooled HTTP connections, and refuses new ones. Bound devices disconnect, and a tailnet-authorized new device that connects over the tailnet stays pending. **tailscaled stopped:** a device configured by hand keeps syncing over the LAN, and no device is added or removed. |
| Listen-only. | S5 | The LAN address refuses connections, the peer reaches the node over the tailnet, and with tailscaled stopped nothing listens. |
| No improper access. | All | Every scenario checks that each node's folder list and connected devices contain exactly what the grants allow. |

## Sources

Syncthing specifications:

- [BEP v1](https://docs.syncthing.net/specs/bep-v1.html): "The encryption and
  authentication layer SHALL use TLS 1.3 or a higher revision". Authentication
  "SHALL be based on the TLS certificate presented at the start of the
  connection".
- [Device IDs](https://docs.syncthing.net/dev/device-ids.html): the device ID
  is the SHA-256 of the certificate in DER form. The local discovery mechanism
  "isn't protected by crypto".
- [Global discovery v3](https://docs.syncthing.net/specs/globaldisco-v3.html):
  only the announcer is authenticated, by its client certificate. Lookups return
  addresses only.
- [Local discovery v4](https://docs.syncthing.net/specs/localdisco-v4.html):
  unauthenticated announcements on port 21027.

Syncthing code:

- `lib/discover/discover.go`: the `Finder` interface.
- `lib/discover/manager.go` and `lib/discover/cache.go`: how finders are added
  and cached.
- `lib/connections/service.go`:
  - `handleConns`, `connectionCheckEarly` and `handleHellos`: what runs after
    TLS for every connection.
  - `validateIdentity`: dialed connections must present the expected device
    ID.
  - `CommitConfiguration`: listener management.
- `lib/connections/relay_listen.go`: relayed connections carry the relay's
  endpoints.
- `lib/model/model.go`:
  - `OnHello`: unknown devices become pending.
  - `ClusterConfig` and `handleAutoAccepts`: folder auto-accept. Without an
    allowlist, auto-accept also shares an existing local folder with a peer that
    merely offers its ID.
  - `handleIntroductions`: introducers add shares.
  - `Request`: data requests require the folder to be shared.
- `lib/config/folderconfiguration.go` (`prepare`): where sharing constraints
  are enforced on every commit, as `ensureNoUntrustedTrustingSharing` already
  does.
- `lib/tlsutil/tlsutil.go`: BEP uses TLS 1.3 only.

Tailscale:

- [Grants](https://tailscale.com/docs/features/access-control/grants),
  [grants syntax](https://tailscale.com/docs/reference/syntax/grants) and
  [application capabilities](https://tailscale.com/docs/features/access-control/grants/grants-app-capabilities):
  - Capabilities are named `{domain}/{path}`.
  - The policy engine does not interpret their values.
  - The values of all matching grants are merged into one list per name.
  - "Application capabilities only apply if the device has the network level
    access". An established BEP connection proves that network access.
- [Sharing](https://tailscale.com/docs/features/sharing): "A machine cannot be
  shared with a tag or accessed by tagged machines on another tailnet".
- [Tags](https://tailscale.com/docs/features/tags) and
  [key expiry](https://tailscale.com/docs/features/access-control/key-expiry).
- LocalAPI client `tailscale.com/client/local`:
  - `WhoIs` returns `apitype.WhoIsResponse{Node, UserProfile, CapMap}`, and
    `ErrPeerNotFound` for addresses outside the netmap. The `CapMap` is
    computed for the node pair (`ipn/ipnlocal/node_backend.go`).
  - `Status` lists peers with `ID` (`StableNodeID`), `DNSName`, `Tags`,
    `TailscaleIPs` and `Expired`.
  - `WatchIPNBus` streams changes. The `NotifyInitialNetMap` bit is rejected
    since tailscaled v1.104. The integration therefore subscribes with
    `NotifyInitialState` and `NotifyPeerChanges`, and reads `Status` again
    after every notification. This works the same across daemon versions.
- [golink](https://github.com/tailscale/golink/blob/d4c752ba2a71793dd8035a5b23746f2c97831c89/golink.go):
  calls `WhoIs(r.RemoteAddr)` for each request and reads
  `tailcfg.UnmarshalCapJSON` for `tailscale.com/cap/golink`. A missing
  capability means no privileges.
- [tsidp](https://github.com/tailscale/tsidp/blob/3c4364fa73369fc641652272b55f67c7fb73fbca/server/appcap.go):
  "deny-all-by-default". An error from `WhoIs` or from parsing the capability
  rejects the request. The rules from all grant values are merged.

Headscale:

- [v0.29.0](https://github.com/juanfont/headscale/releases/tag/v0.29.0) added
  "policy grants with `ip`, `app`, and `via` fields". Custom capability names in
  your own domain pass validation.

Syncthing community discussion of Tailscale:

- Forum, 2020:
  [forum 15293](https://forum.syncthing.net/t/syncthing-tailscale/15293).
  Without integration, users hardcode `tcp://<tailnet-ip>:22000`.
- Forum, 2025:
  [forum 24558](https://forum.syncthing.net/t/the-opposite-of-alwayslocalnet-maybe-alwayswannet/24558).
  calmh "sees no principal blockers" against interface-priority work.
- Forum, 2025:
  [forum 24553](https://forum.syncthing.net/t/tailscale-between-two-synology-nas/24553).
  Tailscale's CGNAT IPv4 addresses are not announced, but its ULA IPv6
  addresses are.
- Forum, 2017:
  [forum 9153](https://forum.syncthing.net/t/use-case-requires-discovery-to-establish-runtime-configurations/9153).
  calmh: "It's obviously insecure to just add whomever connects". That is why
  this design requires an explicit grant.
- [#9998](https://github.com/syncthing/syncthing/issues/9998) (DERP transport)
  and [#10877](https://github.com/syncthing/syncthing/issues/10877) (iroh
  backend) were declined. New transports are not wanted, so this design adds
  none.
