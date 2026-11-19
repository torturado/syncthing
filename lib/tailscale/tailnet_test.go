// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package tailscale

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/views"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/tlsutil"
)

// fakeLocalAPI serves a status that the test controls. Setting an error
// breaks the current IPN bus watch, as when tailscaled stops.
type fakeLocalAPI struct {
	mut    sync.Mutex
	status *ipnstate.Status
	err    error
	down   chan struct{}
	notify chan struct{}
}

func newFakeLocalAPI(st *ipnstate.Status) *fakeLocalAPI {
	return &fakeLocalAPI{status: st, down: make(chan struct{}), notify: make(chan struct{}, 1)}
}

func (f *fakeLocalAPI) Status(context.Context) (*ipnstate.Status, error) {
	f.mut.Lock()
	defer f.mut.Unlock()
	return f.status, f.err
}

func (f *fakeLocalAPI) WatchIPNBus(ctx context.Context, _ ipn.NotifyWatchOpt) (ipnBusWatcher, error) {
	f.mut.Lock()
	defer f.mut.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &fakeWatcher{ctx: ctx, down: f.down, notify: f.notify}, nil
}

func (f *fakeLocalAPI) setStatus(st *ipnstate.Status) {
	f.mut.Lock()
	f.status = st
	f.mut.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

func (f *fakeLocalAPI) stop(err error) {
	f.mut.Lock()
	defer f.mut.Unlock()
	f.err = err
	close(f.down)
}

type fakeWatcher struct {
	ctx    context.Context
	down   chan struct{}
	notify chan struct{}
}

func (w *fakeWatcher) Next() (ipn.Notify, error) {
	select {
	case <-w.notify:
		return ipn.Notify{}, nil
	case <-w.down:
		return ipn.Notify{}, errors.New("tailscaled stopped")
	case <-w.ctx.Done():
		return ipn.Notify{}, w.ctx.Err()
	}
}

func (*fakeWatcher) Close() error { return nil }

func testStatus(peers ...*ipnstate.PeerStatus) *ipnstate.Status {
	st := &ipnstate.Status{
		BackendState: ipn.Running.String(),
		TUN:          true,
		Self:         &ipnstate.PeerStatus{DNSName: "self.tailnet.test."},
		Peer:         map[key.NodePublic]*ipnstate.PeerStatus{},
	}
	for _, p := range peers {
		st.Peer[key.NewNode().Public()] = p
	}
	return st
}

func testPeer(id, ip string, tags ...string) *ipnstate.PeerStatus {
	tagView := views.SliceOf(tags)
	return &ipnstate.PeerStatus{ID: tailcfg.StableNodeID(id), DNSName: id + ".tailnet.test.", TailscaleIPs: []netip.Addr{netip.MustParseAddr(ip)}, Tags: &tagView}
}

// bepServer is a TLS listener that behaves like the BEP listener and records
// whether clients presented a certificate.
type bepServer struct {
	net.Listener
	id protocol.DeviceID

	mut         sync.Mutex
	handshakes  int
	clientCerts int
}

func newBEPServer(t *testing.T) *bepServer {
	t.Helper()
	cert, err := tlsutil.NewCertificateInMemory("syncthing", 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg := tlsutil.SecureDefaultTLS13()
	cfg.Certificates = []tls.Certificate{cert}
	cfg.NextProtos = []string{"bep/1.0"}
	cfg.ClientAuth = tls.RequestClientCert
	lst, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &bepServer{Listener: lst, id: protocol.NewDeviceID(cert.Certificate[0])}
	go func() {
		for {
			conn, err := lst.Accept()
			if err != nil {
				return
			}
			tc := conn.(*tls.Conn)
			_ = tc.Handshake()
			s.mut.Lock()
			s.handshakes++
			s.clientCerts += len(tc.ConnectionState().PeerCertificates)
			s.mut.Unlock()
			tc.Close()
		}
	}()
	t.Cleanup(func() { lst.Close() })
	return s
}

func (s *bepServer) port() uint16 {
	return uint16(s.Addr().(*net.TCPAddr).Port) //nolint:gosec // a listening port fits
}

func startTailnet(t *testing.T, api localAPI, port uint16) *Tailnet {
	t.Helper()
	cfg := config.New(protocol.LocalDeviceID)
	cfg.Options.TailscaleDiscoveryEnabled = true
	wrapper := config.Wrap("", cfg, protocol.LocalDeviceID, events.NoopLogger)
	ourCert, err := tlsutil.NewCertificateInMemory("syncthing", 1)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg := tlsutil.SecureDefaultTLS13()
	tlsCfg.Certificates = []tls.Certificate{ourCert}
	tlsCfg.NextProtos = []string{"bep/1.0"}

	tn := newTailnet(wrapper, api, tlsCfg, port)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tn.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return tn
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTailnetResolvesTaggedPeers(t *testing.T) {
	srv := newBEPServer(t)
	want := []string{fmt.Sprintf("tcp://127.0.0.1:%d", srv.port())}

	tagged := testPeer("n-tagged", "127.0.0.1", PeerTag)
	untagged := testPeer("n-untagged", "127.0.0.2")
	// A node of another tailnet, shared in or shared out to.
	foreign := testPeer("n-foreign", "127.0.0.3", PeerTag)
	foreign.DNSName = "n-foreign.other.ts.net."
	expired := testPeer("n-expired", "127.0.0.4", PeerTag)
	expired.Expired = true
	api := newFakeLocalAPI(testStatus(tagged, untagged, foreign, expired))
	tn := startTailnet(t, api, srv.port())

	waitFor(t, "the tagged peer to be identified", func() bool {
		return slices.Equal(tn.DeviceAddresses(srv.id), want)
	})
	if devices := tn.Devices(); len(devices) != 1 || !slices.Equal(devices[srv.id], want) {
		t.Errorf("Devices() = %v, want only %v", devices, want)
	}
	tn.mut.Lock()
	if _, ok := tn.peers["n-tagged"]; !ok || len(tn.peers) != 1 {
		t.Errorf("peer table %v, want only the tagged, unexpired peer of our tailnet", tn.peers)
	}
	tn.mut.Unlock()
	waitFor(t, "the server to record the probe", func() bool {
		srv.mut.Lock()
		defer srv.mut.Unlock()
		return srv.handshakes > 0
	})
	srv.mut.Lock()
	if srv.clientCerts != 0 {
		t.Errorf("server saw %d client certificates; the probe must not present one", srv.clientCerts)
	}
	srv.mut.Unlock()

	// The peer leaves the tailnet.
	api.setStatus(testStatus(untagged))
	waitFor(t, "the departed peer to be forgotten", func() bool {
		return len(tn.DeviceAddresses(srv.id)) == 0
	})

	// tailscaled stops: nothing resolves and the error is reported.
	api.setStatus(testStatus(tagged))
	waitFor(t, "the peer to come back", func() bool {
		return len(tn.DeviceAddresses(srv.id)) == 1
	})
	api.stop(errors.New("connection refused"))
	waitFor(t, "the failure to be reported", func() bool {
		return tn.Error() != nil && len(tn.Devices()) == 0
	})
}

func TestTailnetRequiresRunningKernelTUN(t *testing.T) {
	for _, st := range []*ipnstate.Status{
		{BackendState: ipn.Stopped.String(), TUN: true},
		{BackendState: ipn.Running.String(), TUN: false},
	} {
		st.Peer = testStatus(testPeer("n1", "127.0.0.1", PeerTag)).Peer
		if peers, err := taggedPeers(st); err == nil || peers != nil {
			t.Errorf("taggedPeers(state %s, TUN %v) = %v, %v; want no peers and an error", st.BackendState, st.TUN, peers, err)
		}
	}
}

func TestProbeRequiresBEPListener(t *testing.T) {
	// A TLS service that never asks for a client certificate is not a BEP
	// listener, and nothing is learned from it.
	cert, err := tlsutil.NewCertificateInMemory("syncthing", 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg := tlsutil.SecureDefaultTLS13()
	cfg.Certificates = []tls.Certificate{cert}
	lst, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lst.Close()
	go func() {
		for {
			conn, err := lst.Accept()
			if err != nil {
				return
			}
			_ = conn.(*tls.Conn).Handshake()
			conn.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if id, err := probeDeviceID(ctx, tlsutil.SecureDefaultTLS13(), lst.Addr().String()); err == nil {
		t.Errorf("probe learned %v from a listener that requests no client certificate", id)
	}
}
