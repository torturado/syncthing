// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package tailscale

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"

	"github.com/syncthing/syncthing/internal/slogutil"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
)

// CapSyncthing is the application capability that a tailnet policy grant
// gives to the nodes that may be Syncthing devices of the destination node.
// Its values are Grants.
const CapSyncthing tailcfg.PeerCapability = "syncthing.net/cap/syncthing"

const whoisTimeout = 5 * time.Second

// A Grant is one value of CapSyncthing. When several grants match a node,
// their folders are merged.
type Grant struct {
	// Folders lists the IDs of the folders that the destination node may
	// share with the source node.
	Folders []string `json:"folders"`
}

// authorization is what the tailnet says about one peer.
type authorization struct {
	nodeID  tailcfg.StableNodeID
	name    string
	folders []string  // sorted, without duplicates
	asked   time.Time // when the answer was asked for
}

var errTailnetUnavailable = errors.New("tailscale is unavailable")

// CheckConnection decides, before the Hello exchange, what the tailnet says
// about an authenticated connection, and returns the node that it authorizes
// the connection for, if any. Devices configured by hand are not affected. A
// device bound to a tailnet node is allowed only while that node holds the
// grant, and its folders follow the grant. An unknown device is added when
// auto-accept is on and the tailnet authorizes it; otherwise it is left to
// become pending, as usual. direct is false for relayed connections, which
// the tailnet can never authorize; established is when the connection's
// handshake completed.
func (t *Tailnet) CheckConnection(ctx context.Context, deviceID protocol.DeviceID, local, remote net.Addr, direct bool, established time.Time) (string, error) {
	dev, known := t.cfg.Device(deviceID)
	if known && dev.TailnetNodeID == "" {
		return "", nil
	}
	if !known && !t.cfg.Options().TailscaleAutoAcceptDevices {
		return "", nil
	}

	auth, err := t.authorizeConnection(ctx, local, remote, direct, established)
	if !known {
		if err != nil {
			slog.DebugContext(ctx, "Unknown device not authorized by the tailnet", deviceID.LogAttr(), slogutil.Address(remote), slogutil.Error(err))
			return "", nil
		}
		t.addDevice(ctx, deviceID, auth)
		return string(auth.nodeID), nil
	}
	if err != nil {
		return "", err
	}
	if err := t.checkBound(ctx, dev, auth); err != nil {
		return "", err
	}
	return string(auth.nodeID), nil
}

// CheckEstablished checks an established connection of a device bound to a
// tailnet node again; its local address was checked when it was set up.
func (t *Tailnet) CheckEstablished(ctx context.Context, deviceID protocol.DeviceID, remote net.Addr, established time.Time) error {
	dev, ok := t.cfg.Device(deviceID)
	if !ok || dev.TailnetNodeID == "" {
		return nil
	}
	auth, err := t.authorizeRemote(ctx, remote, established)
	if err != nil {
		return err
	}
	return t.checkBound(ctx, dev, auth)
}

func (t *Tailnet) checkBound(ctx context.Context, dev config.DeviceConfiguration, auth authorization) error {
	if string(auth.nodeID) != dev.TailnetNodeID {
		return fmt.Errorf("device is bound to tailnet node %s, but the connection belongs to node %s", dev.TailnetNodeID, auth.nodeID)
	}
	return t.applyGrant(ctx, dev.DeviceID, auth)
}

// authorizeConnection requires a direct connection that arrived on one of our
// tailnet addresses, then asks tailscaled who is at the remote end.
func (t *Tailnet) authorizeConnection(ctx context.Context, local, remote net.Addr, direct bool, established time.Time) (authorization, error) {
	if !direct {
		return authorization{}, errors.New("relayed connections are never authorized by the tailnet")
	}
	localAddr, err := netip.ParseAddrPort(local.String())
	if err != nil {
		return authorization{}, fmt.Errorf("local address %s: %w", local, err)
	}
	t.mut.Lock()
	self := t.self
	t.mut.Unlock()
	if !slices.Contains(self, localAddr.Addr().Unmap()) {
		return authorization{}, fmt.Errorf("connection to %s is not on the tailnet", local)
	}
	return t.authorizeRemote(ctx, remote, established)
}

// authorizeRemote asks tailscaled who holds the remote address, which must
// not be one of ours: another user on this host could connect from it.
//
// The node must also have held the address in our peer table since before
// the connection was established. Our table only lags tailscaled, and the
// handshake's replies go to the address's owner, so a node that held the
// address before the handshake and still holds it is the node that completed
// it. Otherwise a socket from a previous owner could be admitted, and its
// buffered index entries applied, under the new owner's grant.
func (t *Tailnet) authorizeRemote(ctx context.Context, remote net.Addr, established time.Time) (authorization, error) {
	remoteAddr, err := netip.ParseAddrPort(remote.String())
	if err != nil {
		return authorization{}, fmt.Errorf("remote address %s: %w", remote, err)
	}
	ip := remoteAddr.Addr().Unmap()
	t.mut.Lock()
	self, domain := t.self, t.domain
	owner, ownerSince := t.ownerLocked(ip)
	t.mut.Unlock()
	if !t.available() {
		return authorization{}, errTailnetUnavailable
	}
	if slices.Contains(self, ip) {
		return authorization{}, errors.New("connection comes from one of our own tailnet addresses")
	}
	if owner == "" {
		return authorization{}, fmt.Errorf("%s is not an address of a tagged node of our tailnet", ip)
	}
	if !ownerSince.Before(established) {
		return authorization{}, fmt.Errorf("node %s holds %s only since %s, after the connection was established", owner, ip, ownerSince.Format(time.RFC3339Nano))
	}
	auth, err := t.whois(ctx, remoteAddr.String(), domain)
	if err != nil {
		return authorization{}, err
	}
	if auth.nodeID != owner {
		return authorization{}, fmt.Errorf("%s changed hands from node %s to node %s", ip, owner, auth.nodeID)
	}
	// The tailnet may have become unavailable while WhoIs was answered.
	if !t.available() {
		return authorization{}, errTailnetUnavailable
	}
	return auth, nil
}

// ownerLocked returns the node that our peer table holds for the address,
// and since when it does.
func (t *Tailnet) ownerLocked(ip netip.Addr) (tailcfg.StableNodeID, time.Time) {
	for id, p := range t.peers {
		if slices.Contains(p.addrs, ip) {
			return id, p.since
		}
	}
	return "", time.Time{}
}

// available reports whether the tailnet state can be trusted: tailscaled
// answers, runs on a kernel TUN, and our own addresses and domain are known.
// Without our domain, nodes of other tailnets cannot be told apart.
func (t *Tailnet) available() bool {
	t.mut.Lock()
	defer t.mut.Unlock()
	return t.err == nil && len(t.self) > 0 && t.domain != ""
}

// NodeAddresses returns the tailnet addresses of a tagged node of our
// tailnet, or nothing if there is no such node or the tailnet is
// unavailable.
func (t *Tailnet) NodeAddresses(node string) []netip.Addr {
	t.mut.Lock()
	defer t.mut.Unlock()
	p, ok := t.peers[tailcfg.StableNodeID(node)]
	if t.err != nil || !ok {
		return nil
	}
	return slices.Clone(p.addrs)
}

func (t *Tailnet) whois(ctx context.Context, addr, domain string) (authorization, error) {
	ctx, cancel := context.WithTimeout(ctx, whoisTimeout)
	defer cancel()
	asked := time.Now()
	who, err := t.api.WhoIs(ctx, addr)
	if err != nil {
		return authorization{}, fmt.Errorf("whois %s: %w", addr, err)
	}
	auth, err := authorizationFrom(who, domain, time.Now())
	auth.asked = asked
	return auth, err
}

// authorizationFrom accepts a tagged node of our own tailnet, named in its
// domain, whose key is valid and that holds CapSyncthing. Anything else is
// refused.
func authorizationFrom(who *apitype.WhoIsResponse, domain string, now time.Time) (authorization, error) {
	n := who.Node
	switch {
	case domain == "":
		return authorization{}, errors.New("our tailnet domain is unknown")
	case n == nil || n.StableID == "":
		return authorization{}, errors.New("whois returned no node")
	case tailnetDomain(n.Name) != domain:
		return authorization{}, fmt.Errorf("tailnet node %s belongs to another tailnet", n.Name)
	case n.Expired || !n.KeyExpiry.IsZero() && !n.KeyExpiry.After(now):
		return authorization{}, errors.New("tailnet node key has expired")
	case !slices.Contains(n.Tags, PeerTag):
		return authorization{}, errors.New("tailnet node is not tagged " + PeerTag)
	}
	if _, ok := who.CapMap[CapSyncthing]; !ok {
		return authorization{}, errors.New("tailnet policy does not grant " + string(CapSyncthing))
	}
	grants, err := tailcfg.UnmarshalCapJSON[Grant](who.CapMap, CapSyncthing)
	if err != nil {
		return authorization{}, fmt.Errorf("invalid %s grant: %w", CapSyncthing, err)
	}
	var folders []string
	for _, g := range grants {
		folders = append(folders, g.Folders...)
	}
	slices.Sort(folders)
	folders = slices.Compact(folders)

	name := n.ComputedName
	if name == "" {
		name, _, _ = strings.Cut(n.Name, ".")
	}
	return authorization{nodeID: n.StableID, name: name, folders: folders}, nil
}

// addDevice adds an unknown device bound to its tailnet node, shared with
// the granted folders that exist here. A node gets at most one device.
func (t *Tailnet) addDevice(ctx context.Context, deviceID protocol.DeviceID, auth authorization) {
	if deviceID == t.cfg.MyID() {
		return
	}
	// Check before modifying, so that the common cases do not commit an
	// unchanged configuration; the check is repeated inside Modify, against
	// the configuration the change applies to.
	if enrollable(t.cfg.RawCopy(), deviceID, auth) != nil {
		return
	}
	// Enrolling writes a grant, so it is serialized with applyGrant.
	t.grantMut.Lock()
	defer t.grantMut.Unlock()
	var added bool
	var refused error
	w, err := t.cfg.Modify(func(cfg *config.Configuration) {
		if refused = enrollable(*cfg, deviceID, auth); refused != nil {
			return
		}
		// No binding may be created while the tailnet is unavailable.
		if !t.available() {
			refused = errTailnetUnavailable
			return
		}
		dev := cfg.Defaults.Device.Copy()
		dev.DeviceID = deviceID
		dev.Name = auth.name
		dev.Addresses = []string{"dynamic"}
		dev.Paused = false
		// A tailnet device must not be able to bring in further devices.
		dev.Introducer = false
		dev.IntroducedBy = protocol.EmptyDeviceID
		// Offers are limited to the granted folders; see TailnetAllowsFolder.
		dev.AutoAcceptFolders = true
		dev.TailnetNodeID = string(auth.nodeID)
		dev.TailnetFolders = auth.folders
		cfg.Devices = append(cfg.Devices, dev)
		shareGrantedFolders(cfg, deviceID, auth.folders)
		added = true
	})
	if err != nil {
		slog.WarnContext(ctx, "Failed to add device authorized by the tailnet", deviceID.LogAttr(), slogutil.Error(err))
		return
	}
	w.Wait()
	if added {
		t.noteGrantLocked(deviceID, auth.asked)
		slog.InfoContext(ctx, "Added device authorized by the tailnet", deviceID.LogAttr(), slog.String("name", auth.name), slog.String("node", string(auth.nodeID)), slog.Any("folders", auth.folders))
	} else if refused != nil {
		slog.DebugContext(ctx, "Not adding device authorized by the tailnet", deviceID.LogAttr(), slogutil.Error(refused))
	}
}

var errNodeBound = errors.New("its tailnet node is already bound to another device")

// enrollable reports why the device cannot be added to cfg, if it cannot:
// auto-accept is off, the device is known or ignored, or its node is bound
// to another device.
func enrollable(cfg config.Configuration, deviceID protocol.DeviceID, auth authorization) error {
	if !cfg.Options.TailscaleAutoAcceptDevices {
		return errors.New("auto-accept is off")
	}
	if slices.ContainsFunc(cfg.IgnoredDevices, func(dev config.ObservedDevice) bool { return dev.ID == deviceID }) {
		return errors.New("the device is ignored")
	}
	for _, dev := range cfg.Devices {
		if dev.DeviceID == deviceID {
			return errors.New("the device is known")
		}
		if dev.TailnetNodeID == string(auth.nodeID) {
			slog.Warn("Not adding device: its tailnet node is already bound to another device", deviceID.LogAttr(), slog.String("node", string(auth.nodeID)))
			return errNodeBound
		}
	}
	return nil
}

// applyGrant makes the folders shared with a bound device match its grant,
// unless a newer answer for the device was applied already.
func (t *Tailnet) applyGrant(ctx context.Context, deviceID protocol.DeviceID, auth authorization) error {
	t.grantMut.Lock()
	defer t.grantMut.Unlock()
	if auth.asked.Before(t.grantSeen[deviceID]) {
		return nil
	}
	if grantApplied(t.cfg.RawCopy(), deviceID, auth.folders) {
		t.noteGrantLocked(deviceID, auth.asked)
		return nil
	}
	w, err := t.cfg.Modify(func(cfg *config.Configuration) {
		for i := range cfg.Devices {
			if cfg.Devices[i].DeviceID == deviceID && cfg.Devices[i].TailnetNodeID == string(auth.nodeID) {
				cfg.Devices[i].TailnetFolders = auth.folders
				shareGrantedFolders(cfg, deviceID, auth.folders)
				return
			}
		}
	})
	if err != nil {
		return fmt.Errorf("applying the tailnet grant: %w", err)
	}
	w.Wait()
	t.noteGrantLocked(deviceID, auth.asked)
	slog.InfoContext(ctx, "Applied tailnet grant", deviceID.LogAttr(), slog.Any("folders", auth.folders))
	return nil
}

// noteGrantLocked records that an answer asked for at asked is applied; the
// record only moves forward.
func (t *Tailnet) noteGrantLocked(deviceID protocol.DeviceID, asked time.Time) {
	if asked.After(t.grantSeen[deviceID]) {
		t.grantSeen[deviceID] = asked
	}
}

func grantApplied(cfg config.Configuration, deviceID protocol.DeviceID, granted []string) bool {
	for _, dev := range cfg.Devices {
		if dev.DeviceID == deviceID && !slices.Equal(dev.TailnetFolders, granted) {
			return false
		}
	}
	for _, f := range cfg.Folders {
		if slices.Contains(granted, f.ID) && !f.SharedWith(deviceID) {
			return false
		}
	}
	return true
}

// shareGrantedFolders shares the granted local folders with the device. The
// configuration itself unshares the folders that are not granted; see
// config.DeviceConfiguration.TailnetAllowsFolder.
func shareGrantedFolders(cfg *config.Configuration, deviceID protocol.DeviceID, granted []string) {
	for i := range cfg.Folders {
		f := &cfg.Folders[i]
		if slices.Contains(granted, f.ID) && !f.SharedWith(deviceID) {
			f.Devices = append(f.Devices, config.FolderDeviceConfiguration{DeviceID: deviceID})
		}
	}
}

// reconcile applies the tailnet to the configuration after a refresh or a
// probe: probed peers that the tailnet authorizes are added when auto-accept
// is on, and the folders of bound devices follow their grants.
func (t *Tailnet) reconcile(ctx context.Context) {
	type probed struct {
		nodeID   tailcfg.StableNodeID
		addr     netip.Addr
		deviceID protocol.DeviceID
	}
	t.mut.Lock()
	peers := make([]probed, 0, len(t.peers))
	for id, p := range t.peers {
		peers = append(peers, probed{id, p.addrs[0], p.deviceID})
	}
	domain := t.domain
	t.mut.Unlock()

	cfg := t.cfg.RawCopy()
	devices := cfg.DeviceMap()
	bound := make(map[string]protocol.DeviceID)
	for _, dev := range cfg.Devices {
		if dev.TailnetNodeID != "" {
			bound[dev.TailnetNodeID] = dev.DeviceID
		}
	}

	for _, p := range peers {
		boundID, isBound := bound[string(p.nodeID)]
		_, known := devices[p.deviceID]
		probedUnknown := p.deviceID != protocol.EmptyDeviceID && !known
		if !isBound && !(probedUnknown && cfg.Options.TailscaleAutoAcceptDevices) {
			continue
		}
		auth, err := t.whois(ctx, p.addr.String(), domain)
		if err != nil || auth.nodeID != p.nodeID {
			continue
		}
		if isBound {
			if err := t.applyGrant(ctx, boundID, auth); err != nil {
				slog.WarnContext(ctx, "Failed to apply tailnet grant", boundID.LogAttr(), slogutil.Error(err))
			}
		} else {
			t.addDevice(ctx, p.deviceID, auth)
		}
	}
}
