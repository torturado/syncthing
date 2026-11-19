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
Peers that are shared in from another tailnet or whose key has expired are left
out.

For each peer, Syncthing learns which device ID it runs. It opens a TLS
connection to port 22000 and checks that the peer proves it holds the
certificate's key. It then aborts the handshake before sending its own
certificate, so a peer that is probed never learns our device ID. Because the
probe aborts, the probed Syncthing logs one "Failed TLS handshake" warning.

A device ID then resolves to `tcp://<tailnet-ip>:22000` for every tailnet
address of the peer that presented it.

### When the table changes

- Syncthing probes a peer again only when the peer appears or its addresses
  change.
- When tailscaled is unavailable, the finder returns nothing and reports the
  error in the discovery status.

### Limitation

The peer's port is assumed to be 22000.

## End-to-end tests

`test/tailscale` runs Syncthing on a real tailnet: Headscale is the control
server, and each node is a Docker container running tailscaled with TUN. It
needs Docker and pulls `headscale/headscale` and `tailscale/tailscale`.

```sh
go test -tags tailscale_e2e -v -timeout 20m ./test/tailscale
```
