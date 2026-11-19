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

- Replacing TLS identity. The device ID stays the SHA-256 of the peer's
  certificate. Every connection is authenticated by TLS exactly as before.
  For devices that the tailnet authorized, tailnet authorization is a second,
  independent condition.
- Running Syncthing as its own tailnet node (tsnet). The integration uses the
  system tailscaled.
- tailscaled in userspace-networking mode. In that mode, inbound connections
  reach Syncthing from `127.0.0.1`, so `WhoIs` cannot identify the peer. The
  integration treats that mode as unavailable.

## Assets

1. Folder contents: confidentiality and integrity.
2. The set of devices that may receive each folder: `devices` and folder
   sharing in the configuration.
3. The device's private key, and with it the device ID.

## Trust assumptions

- **Local tailscaled and its LocalAPI are trusted.** They belong to the same
  trusted computing base as the host that runs Syncthing. Whoever can replace
  the LocalAPI socket (root on Linux, or the user session on the macOS GUI
  build) can already read Syncthing's data.
- **The tailnet policy decides authorization only when the local owner opts
  in** with `tailscaleAutoAcceptDevices`. Enabling the option delegates "may
  this device join, and which folders may it get" to whoever can edit the
  tailnet policy file and own `tag:syncthing`.
- **Tailnet source addresses can be trusted.** A packet that arrives on the
  tailnet interface from address X was sent by the node that holds X in the
  current netmap. WireGuard authenticates the packet, and tailscaled drops
  tailnet source addresses that arrive on other interfaces.
- **Discovery results are hints, never authentication.** This matches the
  existing global and local discovery, which anyone can poison.

## Design rules this model depends on

- **R1. Identity stays TLS.** A tailnet address or name never stands in for a
  device ID. Dialing an address that a Finder returned succeeds only when TLS
  presents the expected certificate.
- **R2. Authorization means `WhoIs` on the actual connection.** The connection's
  remote address goes to LocalAPI `WhoIs`. The answer must show:
  - a node that is not expired and not shared in from another tailnet;
  - the `tag:syncthing` tag;
  - the `syncthing.net/cap/syncthing` capability in `CapMap`.

  The connection's local address must also be one of this node's own tailnet
  addresses.
- **R3. Tailnet-authorized devices are bound to a node, not to an IP.** An
  auto-added device stores the node's `StableNodeID` (`tailscaleNodeID`) and
  the folders that its grant allows (`tailscaleFolders`). Every later
  connection of a bound device must satisfy R2, and the `WhoIs` node must be
  the bound node. A node can be bound to at most one device.
- **R4. The grant lists folders.** The capability value is
  `{"folders": ["<folder ID>", ...]}`. When several grants match, their lists
  are merged. A bound device is shared exactly the local folders that the grant
  lists, and it auto-accepts only listed folders. Other folders are never shared
  with it, and folders it offers outside the list stay pending.
- **R5. Re-checks.** The authorization of every connected bound device is
  checked again whenever the tailnet state changes, and at least every 30
  seconds. A failed check closes the device's connections.
- **R6. Fail closed.** If the LocalAPI cannot be reached, returns an error,
  reports a backend state other than `Running`, or runs without TUN:
  - no device is auto-added;
  - bound devices are refused and disconnected;
  - the Finder returns no addresses;
  - listen-only mode has no listeners.

  It never falls back to listening on all interfaces. Devices that were
  configured by hand, without a binding, behave exactly as without the
  integration.
- **R7. Limited privileges for tailnet devices.** An auto-added device is
  never an introducer, so it cannot add further devices.
- **R8. Identity probes do not disclose anything.** To map a tagged peer to a
  device ID, the Finder opens a TLS connection to `<peer>:22000`. It reads and
  verifies the peer's certificate, `CertificateVerify` and `Finished`, then
  aborts before sending its own certificate. The probe learns a device ID whose
  key the peer has proven it holds. It never reveals our device ID to that peer.

## Scenarios

### S1. A shared or compromised node enters the tailnet

| Case | Safe behaviour | Enforced by |
|---|---|---|
| S1a. A node shared in from another tailnet (node sharing). | It is not discovered and is never authorized. Other tailnets cannot apply our tags to shared nodes, and `WhoIs` reports it as shared in (`Sharer`). | Finder skips sharee and untagged peers. R2 rejects shared-in nodes. |
| S1b. A new node with `tag:syncthing` but no grant. | It can be discovered, but it is not authorized. If it connects, it shows up as a pending device, exactly as an unknown device does today, and it receives no folders. We never dial it unless its device ID is configured by hand. | R2: no capability means no auto-accept. The probe reveals nothing about us (R8). |
| S1c. A node with the grant is compromised. | This is the delegated trust boundary. The node gets the folders listed in its grant and nothing more. It cannot introduce other devices (R7). It cannot obtain other folders: sharing is reconciled to the grant, and auto-accept only accepts listed folders (R4). It cannot impersonate another device, because TLS identity is required (R1). It cannot take over another node's device binding (R3). It cannot register more devices, because each node binds one device. Limit the damage with narrow grants, receive-only folders and versioning. | R1, R3, R4, R7. |
| S1d. An authorized node presents another device's certificate, or cycles certificates. | Without the private key, the TLS handshake fails, and the probe fails too (R8), because `CertificateVerify` is checked. A second device ID from an already bound node is not auto-added and stays pending. | R1, R3, R8. |
| S1e. A node uses a misleading name. | The device name comes from the tailnet: the MagicDNS name that control assigns, never the name in the remote `Hello`. | Auto-accept uses `WhoIs` `Node.ComputedName`. |
| S1f. A peer on the LAN spoofs a tailnet source address. | It is rejected. TCP needs a completed handshake, and replies to tailnet addresses are routed through the tailnet interface. tailscaled drops tailnet sources that arrive on other interfaces. As defence in depth, R2 requires that the local address be one of our tailnet addresses. With `tailscaleListenOnly`, nothing listens on other interfaces at all. | R2, listen-only mode. |

### S2. A node leaves the tailnet, its key expires, or its access changes

| Case | Safe behaviour | Enforced by |
|---|---|---|
| S2a. The node is deleted or logged out (expelled). | Its connections close within seconds, and always within one minute. Future connections from it are refused. The device configuration stays in place, so the device comes back if the same node is authorized again. | IPN bus update, then re-check (R5). R3 requires the bound node to be present. |
| S2b. The node key expires. | Same as S2a while the key is expired. Tagged nodes have key expiry disabled by default, so this mostly affects user nodes. | R2 rejects expired nodes (`Expired`, or `KeyExpiry` in the past). |
| S2c. The tag or the grant is removed. | Connections close within 30 seconds, and later connections are refused. | R5 periodic re-check. R2 requires both the tag and the capability. |
| S2d. The grant's folder list shrinks or grows. | Within 30 seconds the bound device is unshared from folders that are no longer listed, and shared with newly listed folders that exist locally. | R4 reconciliation during the re-check. |
| S2e. The local owner disables `tailscaleAutoAcceptDevices`. | No new devices are auto-added. Bound devices still require tailnet authorization (R3). Turning an option off must not turn them into unconditionally trusted devices. To make a bound device a normal one, clear its `tailscaleNodeID`. | R3 depends on the binding, not on the option. |

### S3. An IP address is reused

The tailnet can assign an address that a deleted node used to hold to a new
node.

- Nothing trusts an address on its own. The Finder rebuilds its address table
  from each netmap and probes a peer again when its address set changes.
- Authorization resolves the address with `WhoIs` at connection time, so it
  sees the current owner. For a bound device the `StableNodeID` must match, so a
  new owner of the old address is refused even if it holds the right
  certificate.
- A device that was configured by hand and dialed at a stale address still has
  to pass TLS (R1).

### S4. tailscaled is stopped, has crashed, or is unhealthy

| Case | Safe behaviour |
|---|---|
| S4a. tailscaled is stopped or crashed: the socket is gone or the IPN bus breaks. | The Finder clears its table and reports an error. Nothing is auto-added, and unknown devices go to pending as today. Bound devices are disconnected and refused. Devices configured by hand keep working through every other address and discovery method. The watcher reconnects with backoff, and normal operation resumes when tailscaled is back. |
| S4b. The backend is not `Running`: it is logged out, needs login or is stopped. | Same as S4a. |
| S4c. Userspace networking (`TUN=false`). | Same as S4a. Peers cannot be identified reliably in this mode. |
| S4d. `tailscaleListenOnly` with no tailnet address available. | No BEP listeners at all, and no fallback to `0.0.0.0`. Listeners come back when an address reappears. |
| S4e. A transient `WhoIs` error on a new connection. | That connection is refused. It is retried on the normal dial or redial schedule. |

### S5. Other threats

- **Discovery poisoning.** A tagged peer may answer probes with a certificate
  that it owns. The Finder then maps that device ID to the peer's addresses, and
  connections still require TLS. A peer cannot present someone else's
  certificate, because of R8. The worst case is a failed dial.
- **Probe noise.** Each successful probe makes the remote Syncthing log one
  "Failed TLS handshake" warning, because the client aborts. Probes run only
  when a peer appears or its address set changes. Failed probes retry with
  exponential backoff, capped at two minutes.
- **Concurrent first connections** from the same unknown device. The device is
  added once. The configuration change checks again, inside the modify
  callback, that the device and the node binding do not exist yet.
- **Tailnet admin compromise.** An attacker who controls the tailnet policy can
  grant the capability to any tagged node. This is the delegated trust. It only
  affects nodes that enabled `tailscaleAutoAcceptDevices`, and only for folders
  that exist on those nodes. Document this risk next to the option.
- **Local LocalAPI impersonation.** See the trust assumptions. Out of scope.

## Success criteria and how they are tested

These tests run against a real tailnet. The control server is Headscale v0.29
or later, which supports `grants` with `app` capabilities. Every node runs a
real tailscaled with TUN in its own Docker container. A unit test covers the
probe and authorization logic with a fake LocalAPI.

| Criterion | Scenario | Test |
|---|---|---|
| A new device with the grant syncs 100 % with no manual steps. | Normal operation | A tagged node with the grant starts with an empty configuration plus the three options, and reaches 100 % completion on the granted folder. |
| A device without the grant receives no folders. | S1b | A tagged node without the grant stays pending on the others. No folder is shared with it, and no file reaches it. |
| An expelled device loses its connection in under a minute. | S2a | `headscale nodes delete`, then wait until the peers report it disconnected. Measure the time. |
| With tailscaled stopped, Syncthing keeps its known devices and accepts no new ones. | S4a | Stop tailscaled on one node. A device configured by hand stays connected over the LAN. A device that is new to that node and authorized by the tailnet connects over the LAN and stays pending. |
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
- `lib/discover/manager.go`: how finders are added and cached.
- `lib/connections/service.go`:
  - `handleConns` and `connectionCheckEarly`: what runs after TLS for every
    connection.
  - `validateIdentity`: dialed connections must present the expected device
    ID.
  - `CommitConfiguration`: listener management.
- `lib/model/model.go`:
  - `OnHello`: unknown devices become pending.
  - `ClusterConfig` and `handleAutoAccepts`: folder auto-accept. Without an
    allowlist, auto-accept also shares an existing local folder with a peer that
    merely offers its ID.
- `lib/tlsutil/tlsutil.go`: BEP uses TLS 1.3 only.

Tailscale:

- [Grants](https://tailscale.com/docs/features/access-control/grants),
  [grants syntax](https://tailscale.com/docs/reference/syntax/grants) and
  [application capabilities](https://tailscale.com/docs/features/access-control/grants/grants-app-capabilities):
  - Capabilities are named `{domain}/{path}`.
  - The policy engine does not interpret their values.
  - The values of all matching grants are merged into one list per name.
  - "Application capabilities only apply if the device has the network level
    access".
- [Sharing](https://tailscale.com/docs/features/sharing): "A machine cannot be
  shared with a tag or accessed by tagged machines on another tailnet".
- [Tags](https://tailscale.com/docs/features/tags) and
  [key expiry](https://tailscale.com/docs/features/access-control/key-expiry).
- LocalAPI client `tailscale.com/client/local`: `WhoIs` returns
  `apitype.WhoIsResponse{Node, UserProfile, CapMap}`. `Status` lists peers with
  `ID` (`StableNodeID`), `Tags`, `TailscaleIPs`, `Expired` and `ShareeNode`.
  `WatchIPNBus` streams changes:
  - The `NotifyInitialNetMap` bit is rejected since tailscaled v1.104.
  - The integration therefore subscribes with `NotifyInitialState` and
    `NotifyPeerChanges`, and reads `Status` again after every notification.
    This works the same across daemon versions.
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
