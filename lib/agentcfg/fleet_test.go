// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package agentcfg

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/tailcfg"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/tailscale"
)

// fakeTailnet stands in for lib/tailscale, whose own tests cover how WhoIs
// answers become capabilities or refusals.
type fakeTailnet struct {
	mut   sync.Mutex
	addrs []netip.Addr
	nodes []tailscale.Node
	caps  map[netip.Addr]tailcfg.PeerCapMap // by remote address
	err   error                             // what Capabilities fails with
}

func (f *fakeTailnet) Addresses() []netip.Addr {
	f.mut.Lock()
	defer f.mut.Unlock()
	return f.addrs
}

func (f *fakeTailnet) Nodes() []tailscale.Node {
	f.mut.Lock()
	defer f.mut.Unlock()
	return f.nodes
}

func (f *fakeTailnet) Capabilities(_ context.Context, remote netip.AddrPort) (tailcfg.PeerCapMap, error) {
	f.mut.Lock()
	defer f.mut.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.caps[remote.Addr()], nil
}

func (f *fakeTailnet) set(fn func(*fakeTailnet)) {
	f.mut.Lock()
	defer f.mut.Unlock()
	fn(f)
}

var loopback = netip.MustParseAddr("127.0.0.1")

func readGrant(access string) tailcfg.PeerCapMap {
	return tailcfg.PeerCapMap{CapAgentcfg: {tailcfg.RawMessage(`{"access":"` + access + `"}`)}}
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	lst, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lst.Close()
	return uint16(lst.Addr().(*net.TCPAddr).Port) //nolint:gosec // a listening port fits
}

// newFleetEnv returns a service with the fleet view on over tn, serving its
// report on a free port.
func newFleetEnv(t *testing.T, tn *fakeTailnet) *testEnv {
	t.Helper()
	env := newTestEnv(t, func(cfg *config.Configuration) { cfg.Options.AgentcfgFleetEnabled = true })
	env.svc = New(env.cfg, env.evLogger, tn)
	env.svc.home = env.home
	env.svc.hostNames = func(context.Context) []string { return []string{"test-host"} }
	env.svc.fleetPort = freePort(t)
	return env
}

func (env *testEnv) reportURL() string {
	return "http://" + netip.AddrPortFrom(loopback, env.svc.fleetPort).String() + fleetReportPath
}

func (env *testEnv) listening() bool {
	env.svc.fleetMut.Lock()
	defer env.svc.fleetMut.Unlock()
	return len(env.svc.fleetServers) > 0
}

func get(t *testing.T, url string) (int, Status, error) {
	t.Helper()
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Get(url)
	if err != nil {
		return 0, Status{}, err
	}
	defer resp.Body.Close()
	var st Status
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, st, nil
}

func TestFleetListenerAuthorizesEachRequest(t *testing.T) {
	tn := &fakeTailnet{addrs: []netip.Addr{loopback}, caps: map[netip.Addr]tailcfg.PeerCapMap{loopback: readGrant("read")}}
	env := newFleetEnv(t, tn)
	env.start(t)
	waitFor(t, "the listener", env.listening)
	waitFor(t, "the first check", func() bool { return env.svc.Status().Error != "" })

	code, st, err := get(t, env.reportURL())
	if err != nil || code != http.StatusOK || !strings.Contains(st.Error, "no shared folder") {
		t.Errorf("granted request = %d, %+v, %v; want 200 and the status", code, st, err)
	}

	refusals := map[string]func(*fakeTailnet){
		"no capability":   func(f *fakeTailnet) { f.caps = nil },
		"other access":    func(f *fakeTailnet) { f.caps[loopback] = readGrant("write") },
		"malformed grant": func(f *fakeTailnet) { f.caps[loopback] = tailcfg.PeerCapMap{CapAgentcfg: {`"read"`}} },
		"another tailnet": func(f *fakeTailnet) {
			f.err = errors.New("tailnet node laptop.other.ts.net. belongs to another tailnet")
		},
		"tailnet is down": func(f *fakeTailnet) { f.err = errors.New("tailscale is unavailable") },
		"other capability": func(f *fakeTailnet) {
			f.caps[loopback] = tailcfg.PeerCapMap{tailscale.CapSyncthing: {`{"access":"read"}`}}
		},
	}
	for name, refuse := range refusals {
		tn.set(func(f *fakeTailnet) {
			f.caps = map[netip.Addr]tailcfg.PeerCapMap{loopback: readGrant("read")}
			f.err = nil
			refuse(f)
		})
		// Every path and method is refused before it is looked at.
		for _, url := range []string{env.reportURL(), strings.TrimSuffix(env.reportURL(), fleetReportPath) + "/rest/system/config"} {
			if code, _, err := get(t, url); err != nil || code != http.StatusForbidden {
				t.Errorf("%s: GET %s = %d, %v; want 403", name, url, code, err)
			}
		}
	}

	// tailscaled becomes unavailable: the tailnet has no addresses, and
	// nothing listens until they come back.
	tn.set(func(f *fakeTailnet) {
		f.caps = map[netip.Addr]tailcfg.PeerCapMap{loopback: readGrant("read")}
		f.err = nil
		f.addrs = nil
	})
	waitFor(t, "the listener to close", func() bool { return !env.listening() })
	if _, _, err := get(t, env.reportURL()); err == nil {
		t.Error("the report is served while the tailnet is unavailable")
	}
	tn.set(func(f *fakeTailnet) { f.addrs = []netip.Addr{loopback} })
	waitFor(t, "the listener to come back", env.listening)
	if code, _, err := get(t, env.reportURL()); err != nil || code != http.StatusOK {
		t.Errorf("after the tailnet came back: %d, %v; want 200", code, err)
	}

	// Turning the fleet view off closes the listener too.
	w, err := env.cfg.Modify(func(cfg *config.Configuration) { cfg.Options.AgentcfgFleetEnabled = false })
	if err != nil {
		t.Fatal(err)
	}
	w.Wait()
	waitFor(t, "the listener to close", func() bool { return !env.listening() })
}

func TestFleetCollectsPeerReports(t *testing.T) {
	// The peer serves its report to the aggregator, which connects from
	// the loopback address.
	peerTailnet := &fakeTailnet{addrs: []netip.Addr{loopback}, caps: map[netip.Addr]tailcfg.PeerCapMap{loopback: readGrant("read")}}
	peer := newFleetEnv(t, peerTailnet)
	peer.start(t)
	waitFor(t, "the peer's listener", peer.listening)
	waitFor(t, "the peer's check", func() bool { return peer.svc.Status().Error != "" })

	aggTailnet := &fakeTailnet{nodes: []tailscale.Node{
		{ID: "n-peer", Name: "peer", Addrs: []netip.Addr{loopback}},
		{ID: "n-silent", Name: "silent", Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.2")}},
	}}
	agg := newFleetEnv(t, aggTailnet)
	agg.svc.fleetPort = peer.svc.fleetPort

	fleet, err := agg.svc.Fleet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet) != 3 || fleet[0].Node != "" || fleet[0].Status == nil {
		t.Fatalf("fleet = %+v; want this device first, then two peers", fleet)
	}
	if m := fleet[1]; m.Name != "peer" || m.Status == nil || !strings.Contains(m.Status.Error, "no shared folder") || m.Error != "" {
		t.Errorf("peer = %+v; want its report", m)
	}
	if m := fleet[2]; m.Name != "silent" || m.Status != nil || m.Error == "" {
		t.Errorf("unreachable peer = %+v; want an error", m)
	}

	// The peer withdraws the grant.
	peerTailnet.set(func(f *fakeTailnet) { f.caps = nil })
	fleet, err = agg.svc.Fleet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m := fleet[1]; m.Status != nil || !strings.Contains(m.Error, "refused") {
		t.Errorf("peer without grant = %+v; want a refusal", m)
	}
}
