// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package tailscale

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
)

func granted(nodeID, name string, grants ...string) *apitype.WhoIsResponse {
	var caps []tailcfg.RawMessage
	for _, g := range grants {
		caps = append(caps, tailcfg.RawMessage(g))
	}
	return &apitype.WhoIsResponse{
		Node:   &tailcfg.Node{StableID: tailcfg.StableNodeID(nodeID), Name: name + ".tailnet.test.", ComputedName: name, Tags: []string{PeerTag}},
		CapMap: tailcfg.PeerCapMap{CapSyncthing: caps},
	}
}

func TestAuthorizationFrom(t *testing.T) {
	now := time.Now()
	ok := granted("n1", "laptop", `{"folders":["photos","docs"]}`, `{"folders":["docs"]}`, `{}`)
	auth, err := authorizationFrom(ok, "tailnet.test", now)
	if err != nil || auth.nodeID != "n1" || auth.name != "laptop" || !slices.Equal(auth.folders, []string{"docs", "photos"}) {
		t.Errorf("authorizationFrom(granted) = %+v, %v; want node n1, name laptop, folders [docs photos]", auth, err)
	}

	noComputedName := granted("n1", "laptop", `{}`)
	noComputedName.Node.ComputedName = ""
	if auth, err := authorizationFrom(noComputedName, "tailnet.test", now); err != nil || auth.name != "laptop" {
		t.Errorf("name without ComputedName = %q, %v; want the first label of Name", auth.name, err)
	}

	refused := map[string]func(*apitype.WhoIsResponse){
		"no node":        func(w *apitype.WhoIsResponse) { w.Node = nil },
		"no node ID":     func(w *apitype.WhoIsResponse) { w.Node.StableID = "" },
		"expired":        func(w *apitype.WhoIsResponse) { w.Node.Expired = true },
		"key expiry":     func(w *apitype.WhoIsResponse) { w.Node.KeyExpiry = now.Add(-time.Second) },
		"other tailnet":  func(w *apitype.WhoIsResponse) { w.Node.Name = "laptop.other.ts.net." },
		"untagged":       func(w *apitype.WhoIsResponse) { w.Node.Tags = []string{"tag:other"} },
		"no capability":  func(w *apitype.WhoIsResponse) { w.CapMap = nil },
		"invalid values": func(w *apitype.WhoIsResponse) { w.CapMap[CapSyncthing] = []tailcfg.RawMessage{`{"folders":"photos"}`} },
	}
	for name, mutate := range refused {
		who := granted("n1", "laptop", `{}`)
		mutate(who)
		if auth, err := authorizationFrom(who, "tailnet.test", now); err == nil {
			t.Errorf("%s: authorized as %+v, want refusal", name, auth)
		}
	}
}

var (
	ourTailnetAddr = &net.TCPAddr{IP: net.ParseIP("100.64.0.1"), Port: 22000}
	lanAddr        = &net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 22000}
	node2Addr      = &net.TCPAddr{IP: net.ParseIP("100.64.0.2"), Port: 40000}
	node3Addr      = &net.TCPAddr{IP: net.ParseIP("100.64.0.3"), Port: 40000}
	node4Addr      = &net.TCPAddr{IP: net.ParseIP("100.64.0.4"), Port: 40000}
)

// newAuthorizingTailnet returns a Tailnet whose state says tailscaled is
// up at 100.64.0.1, over a served configuration with two folders and one
// device configured by hand.
func newAuthorizingTailnet(t *testing.T, autoAccept bool) (*Tailnet, *fakeLocalAPI, config.Wrapper, protocol.DeviceID) {
	t.Helper()
	manual := protocol.DeviceID{9}
	cfg := config.New(protocol.LocalDeviceID)
	cfg.Options.TailscaleAutoAcceptDevices = autoAccept
	cfg.Devices = append(cfg.Devices, config.DeviceConfiguration{DeviceID: manual})
	cfg.Folders = []config.FolderConfiguration{
		{ID: "shared", Path: t.TempDir(), FilesystemType: config.FilesystemTypeFake},
		{ID: "private", Path: t.TempDir(), FilesystemType: config.FilesystemTypeFake},
	}
	wrapper := config.Wrap("", cfg, protocol.LocalDeviceID, events.NoopLogger)
	ctx, cancel := context.WithCancel(context.Background())
	go wrapper.Serve(ctx)
	t.Cleanup(cancel)

	api := newFakeLocalAPI(nil)
	tn := newTailnet(wrapper, api, nil, PeerPort)
	tn.self = []netip.Addr{netip.MustParseAddr("100.64.0.1"), netip.MustParseAddr("fd7a:115c:a1e0::1")}
	tn.domain = "tailnet.test"
	return tn, api, wrapper, manual
}

func sharedWith(cfg config.Wrapper, id protocol.DeviceID) []string {
	var folders []string
	for _, f := range cfg.RawCopy().Folders {
		if f.SharedWith(id) {
			folders = append(folders, f.ID)
		}
	}
	return folders
}

// check runs CheckConnection for a direct connection and returns the node
// it authorized.
func check(t *testing.T, tn *Tailnet, id protocol.DeviceID, local, remote net.Addr) (string, error) {
	t.Helper()
	return tn.CheckConnection(context.Background(), id, local, remote, true)
}

func TestCheckConnectionAddsAuthorizedDevice(t *testing.T) {
	ctx := context.Background()
	tn, api, cfg, manual := newAuthorizingTailnet(t, true)
	api.setWhoIs("100.64.0.2", granted("n2", "laptop", `{"folders":["shared","absent"]}`))
	api.setWhoIs("100.64.0.3", granted("n3", "phone", `{"folders":["shared"]}`))
	api.setWhoIs("100.64.0.4", &apitype.WhoIsResponse{Node: &tailcfg.Node{StableID: "n4", Name: "n4.tailnet.test.", Tags: []string{PeerTag}}})

	devX, devY, devZ := protocol.DeviceID{1}, protocol.DeviceID{2}, protocol.DeviceID{3}

	// An unknown device on an authorized node is added, bound to the node,
	// named after it, and shared with the granted folders that exist.
	if node, err := check(t, tn, devX, ourTailnetAddr, node2Addr); err != nil || node != "n2" {
		t.Fatalf("CheckConnection = %q, %v; want node n2", node, err)
	}
	dev, ok := cfg.Device(devX)
	if !ok {
		t.Fatal("authorized device was not added")
	}
	if dev.Name != "laptop" || dev.TailnetNodeID != "n2" || !slices.Equal(dev.TailnetFolders, []string{"absent", "shared"}) || dev.Introducer || !dev.AutoAcceptFolders {
		t.Errorf("added device %+v; want name laptop, node n2, folders [absent shared], auto-accept, not an introducer", dev)
	}
	if got := sharedWith(cfg, devX); !slices.Equal(got, []string{"shared"}) {
		t.Errorf("added device shared with %v, want [shared]", got)
	}

	// Nothing can share a folder outside the grant with it, not even a
	// direct configuration change.
	w, err := cfg.Modify(func(c *config.Configuration) {
		for i := range c.Folders {
			if c.Folders[i].ID == "private" {
				c.Folders[i].Devices = append(c.Folders[i].Devices, config.FolderDeviceConfiguration{DeviceID: devX})
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Wait()
	if got := sharedWith(cfg, devX); !slices.Equal(got, []string{"shared"}) {
		t.Errorf("after sharing private by hand the device is shared with %v, want [shared]", got)
	}

	// The bound device is refused from another node, even an authorized one,
	// over a relay, and from our own address.
	if _, err := check(t, tn, devX, ourTailnetAddr, node3Addr); err == nil {
		t.Error("bound device accepted from another node")
	}
	if _, err := tn.CheckConnection(ctx, devX, ourTailnetAddr, node2Addr, false); err == nil {
		t.Error("bound device accepted over a relay")
	}
	if _, err := check(t, tn, devX, ourTailnetAddr, &net.TCPAddr{IP: net.ParseIP("100.64.0.1"), Port: 40000}); err == nil {
		t.Error("bound device accepted from our own tailnet address")
	}
	// A second device on the same node is not added.
	if _, err := check(t, tn, devY, ourTailnetAddr, node2Addr); err != nil {
		t.Error(err)
	}
	if _, ok := cfg.Device(devY); ok {
		t.Error("a second device was bound to the same node")
	}
	// A node without the grant does not get its device added.
	if node, err := check(t, tn, devZ, ourTailnetAddr, node4Addr); err != nil || node != "" {
		t.Errorf("CheckConnection for an ungranted node = %q, %v; want no node", node, err)
	}
	if _, ok := cfg.Device(devZ); ok {
		t.Error("device of a node without the grant was added")
	}
	// Connections that did not arrive on the tailnet prove nothing.
	if _, err := check(t, tn, devX, lanAddr, node2Addr); err == nil {
		t.Error("bound device accepted on a non-tailnet address")
	}
	if node, err := tn.CheckConnection(ctx, devZ, ourTailnetAddr, node3Addr, false); err != nil || node != "" {
		t.Errorf("CheckConnection over a relay = %q, %v; want no node", node, err)
	}
	if _, ok := cfg.Device(devZ); ok {
		t.Error("device added from a relayed connection")
	}
	// Devices configured by hand are never affected.
	if node, err := check(t, tn, manual, lanAddr, lanAddr); err != nil || node != "" {
		t.Errorf("device configured by hand: %q, %v", node, err)
	}

	// The grant shrinks: the device stays, its folders follow the grant.
	api.setWhoIs("100.64.0.2", granted("n2", "laptop", `{}`))
	if err := tn.CheckEstablished(ctx, devX, node2Addr); err != nil {
		t.Errorf("device with a grant rejected: %v", err)
	}
	if got := sharedWith(cfg, devX); len(got) != 0 {
		t.Errorf("after the grant shrank the device is shared with %v, want nothing", got)
	}
	// The grant is removed: the connection must close.
	api.setWhoIs("100.64.0.2", &apitype.WhoIsResponse{Node: &tailcfg.Node{StableID: "n2", Name: "n2.tailnet.test.", Tags: []string{PeerTag}}})
	if err := tn.CheckEstablished(ctx, devX, node2Addr); err == nil {
		t.Error("established connection kept after the grant was removed")
	}
}

func TestCheckConnectionFailsClosed(t *testing.T) {
	ctx := context.Background()
	tn, api, cfg, manual := newAuthorizingTailnet(t, true)
	api.setWhoIs("100.64.0.2", granted("n2", "laptop", `{"folders":["shared"]}`))
	bound := protocol.DeviceID{1}
	if _, err := check(t, tn, bound, ourTailnetAddr, node2Addr); err != nil {
		t.Fatal(err)
	}

	// The LocalAPI goes away.
	api.stop(errors.New("connection refused"))
	tn.reset(errors.New("connection refused"))
	if _, err := check(t, tn, bound, ourTailnetAddr, node2Addr); err == nil {
		t.Error("bound device accepted while the LocalAPI is unavailable")
	}
	if err := tn.CheckEstablished(ctx, bound, node2Addr); err == nil {
		t.Error("bound device kept while the LocalAPI is unavailable")
	}
	unknown := protocol.DeviceID{2}
	if node, err := check(t, tn, unknown, ourTailnetAddr, node2Addr); err != nil || node != "" {
		t.Errorf("CheckConnection while unavailable = %q, %v; want no node", node, err)
	}
	if _, ok := cfg.Device(unknown); ok {
		t.Error("device added while the LocalAPI is unavailable")
	}
	if _, err := check(t, tn, manual, lanAddr, lanAddr); err != nil {
		t.Errorf("device configured by hand rejected while the LocalAPI is unavailable: %v", err)
	}
	if _, ok := cfg.Device(bound); !ok {
		t.Error("bound device was removed from the configuration")
	}
}

func TestCheckConnectionWithoutAutoAccept(t *testing.T) {
	tn, api, cfg, _ := newAuthorizingTailnet(t, false)
	api.setWhoIs("100.64.0.2", granted("n2", "laptop", `{"folders":["shared"]}`))
	unknown := protocol.DeviceID{1}
	if _, err := check(t, tn, unknown, ourTailnetAddr, node2Addr); err != nil {
		t.Error(err)
	}
	if _, ok := cfg.Device(unknown); ok {
		t.Error("device added with auto-accept off")
	}
}

func TestCheckConnectionNeverEnrollsIgnoredDevice(t *testing.T) {
	tn, api, cfg, _ := newAuthorizingTailnet(t, true)
	api.setWhoIs("100.64.0.2", granted("n2", "laptop", `{"folders":["shared"]}`))
	ignored := protocol.DeviceID{1}
	w, err := cfg.Modify(func(c *config.Configuration) {
		c.IgnoredDevices = append(c.IgnoredDevices, config.ObservedDevice{ID: ignored})
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Wait()

	if _, err := check(t, tn, ignored, ourTailnetAddr, node2Addr); err != nil {
		t.Error(err)
	}
	if _, ok := cfg.Device(ignored); ok {
		t.Error("an ignored device was added")
	}
	if !cfg.IgnoredDevice(ignored) {
		t.Error("the device is no longer ignored")
	}
}

func TestBoundDeviceStaysBound(t *testing.T) {
	ctx := context.Background()
	tn, api, cfg, _ := newAuthorizingTailnet(t, true)
	api.setWhoIs("100.64.0.2", granted("n2", "laptop", `{"folders":["shared"]}`))
	bound := protocol.DeviceID{1}
	if _, err := check(t, tn, bound, ourTailnetAddr, node2Addr); err != nil {
		t.Fatal(err)
	}

	// It cannot be made an introducer.
	w, err := cfg.Modify(func(c *config.Configuration) {
		for i := range c.Devices {
			if c.Devices[i].DeviceID == bound {
				c.Devices[i].Introducer = true
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Wait()
	if dev, _ := cfg.Device(bound); dev.Introducer {
		t.Error("a bound device became an introducer")
	}

	// An answer asked for before the last one applied never overwrites it.
	newer, err := tn.whois(ctx, "100.64.0.2", "tailnet.test")
	if err != nil {
		t.Fatal(err)
	}
	older := newer
	older.asked = newer.asked.Add(-time.Second)
	older.folders = []string{"private", "shared"}
	newer.folders = nil
	if err := tn.applyGrant(ctx, bound, newer); err != nil {
		t.Fatal(err)
	}
	if err := tn.applyGrant(ctx, bound, older); err != nil {
		t.Fatal(err)
	}
	if got := sharedWith(cfg, bound); len(got) != 0 {
		t.Errorf("an older answer shared %v again", got)
	}
}
