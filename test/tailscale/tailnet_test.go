// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build tailscale_e2e

package tailscale

import (
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
)

// networkOnly lets every node reach every other node; it grants no
// application capability.
const networkOnly = `{
  "tagOwners": {"tag:syncthing": ["syncthing@"]},
  "grants": [{"src": ["*"], "dst": ["*"], "ip": ["*"]}]
}`

func enableDiscovery(n *node) {
	n.modifyConfig(func(cfg *config.Configuration) {
		cfg.Options.TailscaleDiscoveryEnabled = true
	})
}

// TestDiscovery: devices configured by ID only find each other through the
// tailnet, and only when the peer is tagged tag:syncthing.
func TestDiscovery(t *testing.T) {
	h := newHarness(t, networkOnly)
	tagged := h.authKey("tag:syncthing")
	a := h.startNode("node-a", tagged)
	b := h.startNode("node-b", tagged)
	untagged := h.startNode("node-u", h.authKey())

	a.addDevice(b)
	a.addDevice(untagged)
	b.addDevice(a)
	a.addFolder("shared", b)
	b.addFolder("shared", a)

	// With every other discovery method off, nothing connects yet.
	time.Sleep(10 * time.Second)
	if _, ok := a.connectedTo(b); ok {
		t.Fatal("node-a connected to node-b without Tailscale discovery")
	}

	enableDiscovery(a)
	enableDiscovery(b)
	waitFor(t, "node-a to connect to node-b over the tailnet", time.Minute, func() bool {
		_, ok := a.connectedTo(b)
		return ok
	})
	conn, _ := a.connectedTo(b)
	host, _, _ := net.SplitHostPort(conn.Address)
	if !slices.Contains(b.tailnetIPs(), host) {
		t.Errorf("connection address %s is not one of node-b's tailnet addresses %v", conn.Address, b.tailnetIPs())
	}

	var disco map[string]struct {
		Addresses []string `json:"addresses"`
	}
	a.rest(http.MethodGet, "/rest/system/discovery", nil, &disco)
	for _, ip := range b.tailnetIPs() {
		want := "tcp://" + net.JoinHostPort(ip, "22000")
		if !slices.Contains(disco[b.id.String()].Addresses, want) {
			t.Errorf("discovery cache for node-b = %v, missing %s", disco[b.id.String()].Addresses, want)
		}
	}

	a.exec("sh", "-c", "echo hello over the tailnet > /data/shared/hello.txt")
	a.rest(http.MethodPost, "/rest/db/scan?folder=shared", nil, nil)
	waitFor(t, "the file to reach node-b", time.Minute, func() bool {
		content, ok := b.fileContent("/data/shared/hello.txt")
		return ok && content == "hello over the tailnet"
	})

	// node-u is on the tailnet and configured on node-a, but untagged, so
	// Tailscale discovery must not resolve it.
	enableDiscovery(untagged)
	time.Sleep(15 * time.Second)
	if _, ok := a.connectedTo(untagged); ok {
		t.Error("node-a connected to the untagged node-u")
	}
	a.rest(http.MethodGet, "/rest/system/discovery", nil, &disco)
	if _, ok := disco[untagged.id.String()]; ok {
		t.Error("node-u appears in node-a's discovery cache")
	}
}

// grantPolicy lets tagged nodes reach each other on the BEP port and gives
// the syncthing.net/cap/syncthing grant, for folder "shared", only to nodes
// that are also tagged tag:approved.
const grantPolicy = `{
  "tagOwners": {"tag:syncthing": ["syncthing@"], "tag:approved": ["syncthing@"]},
  "grants": [
    {"src": ["tag:syncthing"], "dst": ["tag:syncthing"], "ip": ["tcp:22000"]},
    {"src": ["tag:approved"], "dst": ["tag:syncthing"],
     "app": {"syncthing.net/cap/syncthing": [{"folders": ["shared"]}]}}
  ]
}`

// revokedPolicy is grantPolicy without the application capability.
const revokedPolicy = `{
  "tagOwners": {"tag:syncthing": ["syncthing@"], "tag:approved": ["syncthing@"]},
  "grants": [
    {"src": ["tag:syncthing"], "dst": ["tag:syncthing"], "ip": ["tcp:22000"]}
  ]
}`

func enableAutoAccept(n *node) {
	n.modifyConfig(func(cfg *config.Configuration) {
		cfg.Options.TailscaleDiscoveryEnabled = true
		cfg.Options.TailscaleAutoAcceptDevices = true
	})
}

func deviceOf(cfg config.Configuration, peer *node) (config.DeviceConfiguration, bool) {
	for _, dev := range cfg.Devices {
		if dev.DeviceID == peer.id {
			return dev, true
		}
	}
	return config.DeviceConfiguration{}, false
}

func folderIDs(cfg config.Configuration) []string {
	var ids []string
	for _, f := range cfg.Folders {
		ids = append(ids, f.ID)
	}
	return ids
}

func sharedFolders(cfg config.Configuration, peer *node) []string {
	var ids []string
	for _, f := range cfg.Folders {
		if f.SharedWith(peer.id) {
			ids = append(ids, f.ID)
		}
	}
	return ids
}

// TestAuthorization covers the success criteria of the auto-accept feature
// on a real tailnet; see docs/tailscale/threat-model.md.
func TestAuthorization(t *testing.T) {
	h := newHarness(t, grantPolicy)
	approved := h.authKey("tag:syncthing", "tag:approved")

	a := h.startNode("node-a", approved)
	a.addFolder("shared")
	a.addFolder("private")
	a.exec("sh", "-c", "echo granted data > /data/shared/granted.txt; echo secret > /data/private/secret.txt")
	enableAutoAccept(a)

	t.Run("a new device with the grant syncs with no manual steps", func(t *testing.T) {
		b := h.startNode("node-b", approved)
		enableAutoAccept(b)

		waitFor(t, "node-b to reach 100% on shared", 2*time.Minute, func() bool {
			_, ok := a.connectedTo(b)
			return ok && a.completion("shared", b) == 100
		})
		if content, _ := b.fileContent("/data/shared/granted.txt"); content != "granted data" {
			t.Errorf("node-b has %q in shared/granted.txt", content)
		}
		dev, ok := deviceOf(a.config(), b)
		if !ok || dev.Name != "node-b" || dev.TailnetNodeID == "" {
			t.Errorf("node-a's entry for node-b = %+v; want it named node-b and bound to its node", dev)
		}
		if got := sharedFolders(a.config(), b); !slices.Equal(got, []string{"shared"}) {
			t.Errorf("node-a shares %v with node-b, want only [shared]", got)
		}
		if got := folderIDs(b.config()); !slices.Equal(got, []string{"shared"}) {
			t.Errorf("node-b has folders %v, want only [shared]", got)
		}
		if _, ok := b.fileContent("/data/private/secret.txt"); ok {
			t.Error("the private folder reached node-b")
		}

		t.Run("a device whose grant is revoked loses its connection", func(t *testing.T) {
			// node-b stays on the tailnet, so only node-a's re-check can
			// close the connection.
			start := time.Now()
			h.setPolicy(revokedPolicy)
			waitFor(t, "node-a to drop node-b", time.Minute, func() bool {
				_, ok := a.connectedTo(b)
				return !ok
			})
			t.Logf("node-b was disconnected %v after its grant was revoked", time.Since(start).Round(time.Millisecond))
			if out := a.exec("grep", "-c", "no longer authorized by the tailnet", "/var/log/syncthing.log"); out == "0" {
				t.Error("node-a did not log closing the revoked connection")
			}

			h.setPolicy(grantPolicy)
			waitFor(t, "node-b to be authorized again", 2*time.Minute, func() bool {
				_, ok := a.connectedTo(b)
				return ok
			})
		})

		t.Run("an expelled device loses its connection in under a minute", func(t *testing.T) {
			start := time.Now()
			h.deleteNode(b)
			waitFor(t, "node-a to drop node-b", time.Minute, func() bool {
				_, ok := a.connectedTo(b)
				return !ok
			})
			t.Logf("node-b was disconnected %v after being expelled", time.Since(start).Round(time.Millisecond))
			time.Sleep(20 * time.Second)
			if _, ok := a.connectedTo(b); ok {
				t.Error("the expelled node-b connected again")
			}
		})
	})

	t.Run("a device without the grant receives no folders", func(t *testing.T) {
		c := h.startNode("node-c", h.authKey("tag:syncthing"))
		enableAutoAccept(c)

		// node-c may add node-a, whose grant covers it, and dial it; node-a
		// must keep it pending.
		waitFor(t, "node-c to show up as pending on node-a", 2*time.Minute, func() bool {
			_, ok := a.pendingDevices()[c.id.String()]
			return ok
		})
		time.Sleep(10 * time.Second)
		if _, ok := deviceOf(a.config(), c); ok {
			t.Error("node-a added node-c, which has no grant")
		}
		if _, ok := a.connectedTo(c); ok {
			t.Error("node-a is connected to node-c")
		}
		if got := folderIDs(c.config()); len(got) != 0 {
			t.Errorf("node-c has folders %v, want none", got)
		}
	})

	t.Run("without the LocalAPI, bound devices disconnect and no new one is accepted", func(t *testing.T) {
		d := h.startNode("node-d", approved)
		enableAutoAccept(d)
		waitFor(t, "node-a to connect to node-d", 2*time.Minute, func() bool {
			_, ok := a.connectedTo(d)
			return ok
		})

		// tailscaled keeps carrying traffic, but Syncthing can no longer
		// ask it anything.
		a.cutLocalAPI()
		waitFor(t, "node-a to drop the bound node-d", time.Minute, func() bool {
			_, ok := a.connectedTo(d)
			return !ok
		})
		if !tailnetErrored(a) {
			t.Error("node-a does not report the LocalAPI failure")
		}

		// node-e is authorized and reaches node-a over the tailnet.
		e := h.startNode("node-e", approved)
		enableAutoAccept(e)
		waitFor(t, "node-e to show up as pending on node-a", 2*time.Minute, func() bool {
			_, ok := a.pendingDevices()[e.id.String()]
			return ok
		})
		if _, ok := deviceOf(a.config(), e); ok {
			t.Error("node-a accepted a new device without the LocalAPI")
		}
		if _, ok := a.connectedTo(d); ok {
			t.Error("node-a reconnected to the bound node-d without the LocalAPI")
		}

		a.restoreLocalAPI()
		waitFor(t, "node-a to reconnect to node-d once the LocalAPI is back", 2*time.Minute, func() bool {
			_, ok := a.connectedTo(d)
			return ok
		})

		t.Run("a device whose key expires loses its connection", func(t *testing.T) {
			start := time.Now()
			h.expireNode(d)
			waitFor(t, "node-a to drop node-d", time.Minute, func() bool {
				_, ok := a.connectedTo(d)
				return !ok
			})
			t.Logf("node-d was disconnected %v after its key expired", time.Since(start).Round(time.Millisecond))
			time.Sleep(20 * time.Second)
			if _, ok := a.connectedTo(d); ok {
				t.Error("node-d connected again with an expired key")
			}
		})
	})

	t.Run("with tailscaled stopped, known devices keep working and no new one is accepted", func(t *testing.T) {
		// node-k is configured by hand on node-a and only reachable on the
		// Docker network.
		k := h.startNode("node-k", "")
		a.modifyConfig(func(cfg *config.Configuration) {
			dev := cfg.Defaults.Device.Copy()
			dev.DeviceID, dev.Name, dev.Addresses = k.id, "node-k", []string{"tcp://" + k.lanIP() + ":22000"}
			cfg.Devices = append(cfg.Devices, dev)
		})
		k.modifyConfig(func(cfg *config.Configuration) {
			dev := cfg.Defaults.Device.Copy()
			dev.DeviceID, dev.Name, dev.Addresses = a.id, "node-a", []string{"tcp://" + a.lanIP() + ":22000"}
			cfg.Devices = append(cfg.Devices, dev)
		})
		a.addFolder("lan", k)
		k.addFolder("lan", a)
		waitFor(t, "node-a to connect to node-k", time.Minute, func() bool {
			_, ok := a.connectedTo(k)
			return ok
		})
		devicesBefore := len(a.config().Devices)

		a.exec("pkill", "tailscaled")
		waitFor(t, "node-a to report that tailscaled is gone", time.Minute, func() bool {
			return tailnetErrored(a)
		})

		// A tailnet-authorized device that knows node-a only by its LAN
		// address tries to join.
		n := h.startNode("node-n", approved)
		n.modifyConfig(func(cfg *config.Configuration) {
			dev := cfg.Defaults.Device.Copy()
			dev.DeviceID, dev.Name, dev.Addresses = a.id, "node-a", []string{"tcp://" + a.lanIP() + ":22000"}
			cfg.Devices = append(cfg.Devices, dev)
		})
		waitFor(t, "node-n to show up as pending on node-a", 2*time.Minute, func() bool {
			_, ok := a.pendingDevices()[n.id.String()]
			return ok
		})
		if _, ok := deviceOf(a.config(), n); ok {
			t.Error("node-a accepted a new device while tailscaled was stopped")
		}
		if got := len(a.config().Devices); got != devicesBefore {
			t.Errorf("node-a has %d devices, had %d before tailscaled stopped", got, devicesBefore)
		}

		if _, ok := a.connectedTo(k); !ok {
			t.Fatal("node-a lost node-k when tailscaled stopped")
		}
		a.exec("sh", "-c", "echo still syncing > /data/lan/after.txt")
		a.rest(http.MethodPost, "/rest/db/scan?folder=lan", nil, nil)
		waitFor(t, "the file to reach node-k", time.Minute, func() bool {
			content, ok := k.fileContent("/data/lan/after.txt")
			return ok && content == "still syncing"
		})
	})
}

// tailnetErrored reports whether node's discovery status shows a Tailscale
// error.
func tailnetErrored(n *node) bool {
	var status struct {
		DiscoveryStatus map[string]struct {
			Error *string `json:"error"`
		} `json:"discoveryStatus"`
	}
	n.rest(http.MethodGet, "/rest/system/status", nil, &status)
	return status.DiscoveryStatus["tailscale"].Error != nil
}

// TestAddressReuse: a new node that receives an expelled node's tailnet
// addresses, and even presents the expelled device's certificate, is not
// treated as the expelled device.
func TestAddressReuse(t *testing.T) {
	// A /30 and a /126 with random allocation leave exactly one free
	// address per family once a node is deleted, so the next node gets it.
	h := newHarnessWithPrefixes(t, grantPolicy, "100.64.0.0/30", "fd7a:115c:a1e0::/126", "random")
	approved := h.authKey("tag:syncthing", "tag:approved")
	a := h.startNode("node-a", approved)
	a.addFolder("shared")
	enableAutoAccept(a)
	b := h.startNode("node-b", approved)
	enableAutoAccept(b)
	waitFor(t, "node-a to connect to node-b", 2*time.Minute, func() bool {
		_, ok := a.connectedTo(b)
		return ok
	})
	bAddrs := b.tailnetIPs()

	h.deleteNode(b)
	waitFor(t, "node-a to drop node-b", time.Minute, func() bool {
		_, ok := a.connectedTo(b)
		return !ok
	})
	b.exec("pkill", "syncthing")

	// node-c takes over node-b's addresses and its Syncthing identity.
	c := h.startNodeWithIdentity("node-c", approved, b)
	if got := c.tailnetIPs(); !slices.Equal(got, bAddrs) {
		t.Fatalf("node-c got addresses %v, want node-b's %v", got, bAddrs)
	}
	enableAutoAccept(c)
	time.Sleep(45 * time.Second)
	if _, ok := a.connectedTo(b); ok {
		t.Fatal("node-a accepted node-b's device from the node that reused its addresses")
	}
	if out, _ := h.dockerErr("exec", a.container, "grep", "-c", "but the connection belongs to node", "/var/log/syncthing.log"); out == "0" || out == "" {
		t.Error("node-a did not refuse the reused address for the bound device")
	}
}
