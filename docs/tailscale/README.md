# Tailscale integration

Syncthing can use the local tailscaled to find its peers on a tailnet. The
device ID stays the identity: every connection is still authenticated with
TLS, exactly as without the integration. The security behaviour is specified
in [threat-model.md](threat-model.md).

## Requirements

- tailscaled runs on the same host, in the `Running` state, with a kernel TUN
  device. Userspace networking is not supported, because it hides peer
  addresses.
- Syncthing can read the LocalAPI socket (on Linux,
  `/var/run/tailscale/tailscaled.sock`). Read access is enough.
- The Syncthing nodes are tagged `tag:syncthing`, and the policy allows them to
  reach each other on TCP port 22000:

  ```json
  {
    "tagOwners": {"tag:syncthing": ["autogroup:admin"]},
    "grants": [
      {"src": ["tag:syncthing"], "dst": ["tag:syncthing"], "ip": ["tcp:22000"]}
    ]
  }
  ```

## Discovery

Set `tailscaleDiscoveryEnabled` under *Actions → Advanced → Options*, or the
`<tailscaleDiscoveryEnabled>` option in `config.xml`.

### How it works

Syncthing follows the tailscaled IPN bus and keeps a table of tagged peers.
Two kinds of peer are left out:

- peers whose MagicDNS name belongs to another tailnet, which are nodes shared
  in or shared out;
- peers whose key has expired.

For each peer, Syncthing learns which device ID it runs. It opens a TLS
connection to port 22000 on each of the peer's addresses in turn, and checks
that the peer proves it holds the certificate's key. It then aborts the
handshake when the peer asks for a client certificate, before sending one, so a
peer that is probed never learns our device ID. Because the probe aborts, the
probed Syncthing logs one "Failed TLS handshake" warning.

A device ID then resolves to `tcp://<tailnet-ip>:22000` for every tailnet
address of the peer that presented it.

### When the table changes

- Syncthing probes a peer again only when the peer appears or its addresses
  change.
- When tailscaled is unavailable, the finder returns nothing and reports the
  error in the discovery status.

### Limitation

The peer's port is assumed to be 22000.

## Accepting devices authorized by the tailnet

Set `tailscaleAutoAcceptDevices` to let the tailnet policy decide which new
devices may join and which folders they get. This delegates the decision to
whoever can edit the policy and own `tag:syncthing`, so only enable it on a
tailnet whose administrators you trust with your folders.

Grant the `syncthing.net/cap/syncthing` application capability. Its value
lists the folders that the destination node may share with the source node:

```json
{
  "tagOwners": {"tag:syncthing": ["autogroup:admin"]},
  "grants": [
    {"src": ["tag:syncthing"], "dst": ["tag:syncthing"], "ip": ["tcp:22000"]},
    {"src": ["tag:syncthing"], "dst": ["tag:syncthing"],
     "app": {"syncthing.net/cap/syncthing": [{"folders": ["photos", "docs"]}]}}
  ]
}
```

Grants are read from both ends, so write them for both directions. The
symmetric grant above covers both.

### When a device is added

A device that Syncthing does not know connects, or a probe finds it on the
tailnet. It is added only if the connection is direct TCP, not relayed and not
through a proxy, arrived on one of our tailnet addresses, and does not come
from one of them. Its remote address must also still belong to the node that
our peer table holds for it. `WhoIs` must
also show all of the following:

- a node named in our own tailnet's domain;
- a key that has not expired;
- `tag:syncthing`;
- the capability, with a value that parses.

Anything else leaves the device pending, as before. A device added this way:

- is named after its MagicDNS name;
- is bound to its tailnet node (`tailnetNodeID`);
- is never an introducer;
- is shared the granted folders that exist locally;
- auto-accepts only granted folders that it offers.

The configuration refuses to share any other folder with it, whether from the
GUI, an introducer or auto-accept. A node can be bound to only one device.

### While the device is bound

The node that authorized a connection is recorded on that connection. A bound
device is admitted only on a connection authorized for its node, and it is
only dialed over TCP at that node's tailnet addresses. Its
established connections go through `WhoIs` again whenever the tailnet changes,
and at least every 30 seconds. When a check fails, they are closed.

The device stays in the configuration and reconnects if its node is authorized
again. Removing a folder from the grant unshares it.

If the LocalAPI cannot be reached, bound devices are refused and no device is
added. Devices configured by hand are never affected. To turn a bound device
into an ordinary one, clear its `tailnetNodeID` in the advanced settings.

## End-to-end tests

`test/tailscale` runs Syncthing on a real tailnet:

- Headscale is the control server.
- A DERP relay runs alongside it.
- Each node is a Docker container running tailscaled with TUN.

It needs Docker, pulls `headscale/headscale` and `tailscale/tailscale`, and
builds `derper` from the same Tailscale release.

```sh
go test -tags tailscale_e2e -v -timeout 20m ./test/tailscale
```
