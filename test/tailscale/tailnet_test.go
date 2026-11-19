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
