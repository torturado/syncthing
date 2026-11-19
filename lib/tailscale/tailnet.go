// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Package tailscale integrates Syncthing with the local tailscaled. It keeps a
// table of the tailnet peers tagged tag:syncthing, learns which device ID each
// of them presents, resolves device IDs to tailnet addresses, and, when
// enabled, lets the tailnet policy authorize devices.
//
// The security behaviour is specified in docs/tailscale/threat-model.md.
package tailscale

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"

	"github.com/syncthing/syncthing/internal/slogutil"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
)

const (
	// PeerTag marks the tailnet nodes that run Syncthing.
	PeerTag = "tag:syncthing"
	// PeerPort is the BEP port used on tailnet peers.
	PeerPort uint16 = 22000

	refreshInterval = 30 * time.Second
	refreshDebounce = time.Second
	statusTimeout   = 10 * time.Second
	probeTimeout    = 10 * time.Second
	probeParallel   = 4
	probeRetryMin   = 15 * time.Second
	probeRetryMax   = 2 * time.Minute
	// Bounds the exponent of the probe backoff, so the shift cannot
	// overflow; probeRetryMax is reached well before.
	probeMaxDoublings = 8
	watchRetryMin     = time.Second
	watchRetryMax     = 30 * time.Second
)

// localAPI is the part of the tailscaled LocalAPI that the integration uses.
type localAPI interface {
	Status(ctx context.Context) (*ipnstate.Status, error)
	WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)
	WatchIPNBus(ctx context.Context, mask ipn.NotifyWatchOpt) (ipnBusWatcher, error)
}

type ipnBusWatcher interface {
	Next() (ipn.Notify, error)
	Close() error
}

// localClient adapts the LocalAPI client, which talks to tailscaled on its
// default socket, to localAPI.
type localClient struct {
	local.Client
}

func (c *localClient) WatchIPNBus(ctx context.Context, mask ipn.NotifyWatchOpt) (ipnBusWatcher, error) {
	w, err := c.Client.WatchIPNBus(ctx, mask)
	if err != nil {
		return nil, err
	}
	return w, nil
}

// Tailnet tracks the Syncthing peers on the local tailnet while any part of
// the integration is in use. It is a suture service.
type Tailnet struct {
	cfg           config.Wrapper
	api           localAPI
	tlsCfg        *tls.Config
	port          uint16
	configChanged chan struct{}
	updates       chan struct{}

	mut     sync.Mutex
	err     error
	self    []netip.Addr // our tailnet addresses; empty while unavailable
	domain  string       // our tailnet's MagicDNS domain
	peers   map[tailcfg.StableNodeID]*peer
	probing int // probes in flight
}

type peer struct {
	addrs     []netip.Addr      // tailnet addresses, IPv4 first; never modified
	deviceID  protocol.DeviceID // learned by probing; empty until then
	probing   bool
	failures  int
	nextProbe time.Time
}

// New returns a Tailnet that talks to tailscaled on its default LocalAPI
// socket. tlsCfg is the BEP TLS configuration; probes never send its
// certificate.
func New(cfg config.Wrapper, tlsCfg *tls.Config) *Tailnet {
	return newTailnet(cfg, &localClient{}, tlsCfg, PeerPort)
}

func newTailnet(cfg config.Wrapper, api localAPI, tlsCfg *tls.Config, port uint16) *Tailnet {
	return &Tailnet{
		cfg:           cfg,
		api:           api,
		tlsCfg:        tlsCfg,
		port:          port,
		configChanged: make(chan struct{}, 1),
		updates:       make(chan struct{}, 1),
	}
}

func (t *Tailnet) Serve(ctx context.Context) error {
	t.cfg.Subscribe(t)
	defer t.cfg.Unsubscribe(t)
	defer t.reset(nil)

	retry := watchRetryMin
	for {
		var err error
		if t.enabled() {
			var connected bool
			connected, err = t.watch(ctx)
			if connected {
				retry = watchRetryMin
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		t.reset(err)

		var wait <-chan time.Time
		if err != nil {
			wait = time.After(retry)
			retry = min(retry*2, watchRetryMax)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.configChanged:
		case <-wait:
		}
	}
}

// watch follows the IPN bus until it fails, the context ends or discovery
// is disabled. connected reports whether tailscaled answered at least once.
func (t *Tailnet) watch(ctx context.Context) (connected bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// tailscaled 1.104 and later reject NotifyInitialNetMap, so rather than
	// parsing the netmap we treat every notification as "something changed"
	// and read the state again with Status.
	w, err := t.api.WatchIPNBus(ctx, ipn.NotifyInitialState|ipn.NotifyPeerChanges)
	if err != nil {
		return false, fmt.Errorf("watch tailscaled: %w", err)
	}
	defer w.Close()

	notified := make(chan struct{}, 1)
	watchErr := make(chan error, 1)
	go func() {
		for {
			if _, err := w.Next(); err != nil {
				watchErr <- err
				return
			}
			select {
			case notified <- struct{}{}:
			default:
			}
		}
	}()

	probed := make(chan struct{}, 1)
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	var debounce <-chan time.Time
	refresh := true
	for {
		if refresh {
			if err := t.refresh(ctx); err != nil {
				return connected, err
			}
			if !connected {
				slog.InfoContext(ctx, "Connected to tailscaled")
			}
			connected = true
			refresh = false
		}
		t.startProbes(ctx, probed)
		t.reconcile(ctx)

		select {
		case <-ctx.Done():
			return connected, nil
		case err := <-watchErr:
			return connected, fmt.Errorf("watch tailscaled: %w", err)
		case <-t.configChanged:
			if !t.enabled() {
				return connected, nil
			}
		case <-notified:
			if debounce == nil {
				debounce = time.After(refreshDebounce)
			}
		case <-debounce:
			debounce = nil
			refresh = true
		case <-ticker.C:
			refresh = true
		case <-probed:
		}
	}
}

func (t *Tailnet) refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	st, err := t.api.Status(ctx)
	if err != nil {
		return fmt.Errorf("query tailscaled status: %w", err)
	}
	peers, err := taggedPeers(st)

	t.mut.Lock()
	defer t.mut.Unlock()
	defer t.notifyUpdate()
	t.setErrLocked(err)
	t.self = nil
	if err == nil {
		t.self = slices.Clone(st.TailscaleIPs)
		t.domain = tailnetDomain(st.Self.DNSName)
	}
	old := t.peers
	t.peers = make(map[tailcfg.StableNodeID]*peer, len(peers))
	for id, addrs := range peers {
		if p, ok := old[id]; ok && slices.Equal(p.addrs, addrs) {
			t.peers[id] = p
			continue
		}
		// New peer, or one whose addresses changed (possibly a reused
		// address): learn its device ID again.
		t.peers[id] = &peer{addrs: addrs}
	}
	return nil
}

// taggedPeers returns the addresses of the usable tagged peers, or an error
// if tailscaled is in a state where peers cannot be trusted.
func taggedPeers(st *ipnstate.Status) (map[tailcfg.StableNodeID][]netip.Addr, error) {
	if st.BackendState != ipn.Running.String() {
		return nil, fmt.Errorf("tailscale is not running (state %s)", st.BackendState)
	}
	if !st.TUN {
		return nil, errors.New("tailscaled uses userspace networking, which cannot identify peers")
	}
	if st.Self == nil {
		return nil, errors.New("tailscaled reported no node of its own")
	}
	domain := tailnetDomain(st.Self.DNSName)
	peers := make(map[tailcfg.StableNodeID][]netip.Addr)
	for _, ps := range st.Peer {
		// Nodes shared in from, or shared out to, another tailnet are named
		// in that tailnet's domain.
		if tailnetDomain(ps.DNSName) != domain || ps.Expired || len(ps.TailscaleIPs) == 0 {
			continue
		}
		if ps.Tags == nil || !ps.Tags.ContainsFunc(func(tag string) bool { return tag == PeerTag }) {
			continue
		}
		addrs := slices.Clone(ps.TailscaleIPs)
		slices.SortFunc(addrs, netip.Addr.Compare)
		peers[ps.ID] = addrs
	}
	return peers, nil
}

// tailnetDomain returns the tailnet part of a node's MagicDNS name, such as
// "example.ts.net" for "laptop.example.ts.net.".
func tailnetDomain(fqdn string) string {
	_, domain, _ := strings.Cut(strings.TrimSuffix(fqdn, "."), ".")
	return domain
}

func (t *Tailnet) startProbes(ctx context.Context, probed chan<- struct{}) {
	now := time.Now()
	t.mut.Lock()
	defer t.mut.Unlock()
	for _, p := range t.peers {
		if t.probing >= probeParallel {
			return
		}
		if p.deviceID != protocol.EmptyDeviceID || p.probing || now.Before(p.nextProbe) {
			continue
		}
		p.probing = true
		t.probing++
		go t.probe(ctx, p, probed)
	}
}

func (t *Tailnet) probe(ctx context.Context, p *peer, probed chan<- struct{}) {
	// Try each address in turn, each with its own deadline: a peer may
	// serve BEP on only one family, and an address that drops packets must
	// not use up the time of the next one.
	var id protocol.DeviceID
	var addr string
	var err error
	for _, ip := range p.addrs {
		addr = netip.AddrPortFrom(ip, t.port).String()
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		id, err = probeDeviceID(probeCtx, t.tlsCfg, addr)
		cancel()
		if err == nil {
			break
		}
	}

	t.mut.Lock()
	t.probing--
	p.probing = false
	if err != nil {
		p.failures = min(p.failures+1, probeMaxDoublings)
		p.nextProbe = time.Now().Add(min(probeRetryMin<<(p.failures-1), probeRetryMax))
		slog.DebugContext(ctx, "Tailnet peer identity probe failed", slogutil.Address(addr), slogutil.Error(err))
	} else {
		p.deviceID = id
		p.failures = 0
		slog.DebugContext(ctx, "Tailnet peer identified", id.LogAttr(), slogutil.Address(addr))
	}
	t.mut.Unlock()

	select {
	case probed <- struct{}{}:
	default:
	}
}

// reset forgets all peers and records why the tailnet is unavailable; a
// nil error means the integration is simply off.
func (t *Tailnet) reset(err error) {
	t.mut.Lock()
	defer t.mut.Unlock()
	defer t.notifyUpdate()
	t.setErrLocked(err)
	t.self = nil
	t.peers = nil
}

func (t *Tailnet) notifyUpdate() {
	select {
	case t.updates <- struct{}{}:
	default:
	}
}

// Updates is signalled whenever the tailnet state has been read again, at
// least every refreshInterval while the integration is in use, so that
// established connections can be checked again.
func (t *Tailnet) Updates() <-chan struct{} {
	return t.updates
}

func (t *Tailnet) setErrLocked(err error) {
	if err != nil && (t.err == nil || t.err.Error() != err.Error()) {
		slog.Warn("Tailscale integration unavailable", slogutil.Error(err))
	}
	t.err = err
}

func (t *Tailnet) enabled() bool {
	return inUse(t.cfg.RawCopy())
}

// inUse reports whether the configuration needs the tailnet: for discovery,
// for auto-accept, or to keep checking devices that the tailnet authorized.
func inUse(cfg config.Configuration) bool {
	if cfg.Options.TailscaleDiscoveryEnabled || cfg.Options.TailscaleAutoAcceptDevices {
		return true
	}
	return slices.ContainsFunc(cfg.Devices, func(dev config.DeviceConfiguration) bool {
		return dev.TailnetNodeID != ""
	})
}

// DeviceAddresses returns the BEP addresses of the tailnet peers that
// presented the given device ID.
func (t *Tailnet) DeviceAddresses(id protocol.DeviceID) []string {
	t.mut.Lock()
	defer t.mut.Unlock()
	var addrs []string
	for _, p := range t.peers {
		if p.deviceID == id {
			addrs = append(addrs, t.bepAddresses(p)...)
		}
	}
	return addrs
}

// Devices returns the BEP addresses of every device identified on the
// tailnet.
func (t *Tailnet) Devices() map[protocol.DeviceID][]string {
	t.mut.Lock()
	defer t.mut.Unlock()
	devices := make(map[protocol.DeviceID][]string)
	for _, p := range t.peers {
		if p.deviceID != protocol.EmptyDeviceID {
			devices[p.deviceID] = append(devices[p.deviceID], t.bepAddresses(p)...)
		}
	}
	return devices
}

func (t *Tailnet) bepAddresses(p *peer) []string {
	addrs := make([]string, len(p.addrs))
	for i, addr := range p.addrs {
		addrs[i] = "tcp://" + netip.AddrPortFrom(addr, t.port).String()
	}
	return addrs
}

// Error returns why the tailnet is currently unavailable, if it is.
func (t *Tailnet) Error() error {
	t.mut.Lock()
	defer t.mut.Unlock()
	return t.err
}

func (t *Tailnet) CommitConfiguration(from, to config.Configuration) bool {
	if inUse(from) != inUse(to) {
		select {
		case t.configChanged <- struct{}{}:
		default:
		}
	}
	return true
}

func (*Tailnet) String() string {
	return "tailscale"
}
