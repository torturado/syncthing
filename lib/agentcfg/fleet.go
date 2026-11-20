// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package agentcfg

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"time"

	"tailscale.com/tailcfg"

	"github.com/syncthing/syncthing/internal/slogutil"
	"github.com/syncthing/syncthing/lib/tailscale"
)

const (
	// FleetPort is the TCP port of the tailnet-only report listener on
	// every tailnet address of the device.
	FleetPort uint16 = 22100
	// CapAgentcfg is the application capability that a tailnet policy grant
	// gives to the nodes that may read a device's report. Its values are
	// fleetGrants.
	CapAgentcfg tailcfg.PeerCapability = "github.com/torturado/cap/agentcfg"

	fleetReportPath = "/agentcfg/report"
	// The tailnet's Updates channel has a single consumer, the connection
	// service, so the listeners follow the tailnet addresses by polling.
	fleetFollowInterval = 2 * time.Second
	fleetPeerTimeout    = 5 * time.Second
	fleetMaxResponse    = 1 << 20
)

var errFleetDisabled = errors.New("the agentcfg fleet view is disabled")

// Tailnet is the part of the Tailscale integration that the fleet view uses.
type Tailnet interface {
	// Addresses returns our tailnet addresses; empty while unavailable.
	Addresses() []netip.Addr
	// Nodes returns the tagged peers of our tailnet.
	Nodes() []tailscale.Node
	// Capabilities returns what the tailnet policy grants the tagged node
	// of our tailnet at remote towards us.
	Capabilities(ctx context.Context, remote netip.AddrPort) (tailcfg.PeerCapMap, error)
}

// fleetGrant is one value of CapAgentcfg.
type fleetGrant struct {
	// Access is "read" for reading the report; nothing else is granted.
	Access string `json:"access"`
}

type fleetServer struct {
	srv  *http.Server
	addr net.Addr
}

func (s *Service) fleetEnabled() bool {
	opts := s.cfg.Options()
	return opts.AgentcfgEnabled && opts.AgentcfgFleetEnabled && s.tailnet != nil
}

// serveFleet keeps one report listener on each of our tailnet addresses
// while the fleet view is enabled, and none while the tailnet is
// unavailable.
func (s *Service) serveFleet(ctx context.Context) error {
	defer s.updateFleetListeners(ctx, nil)
	ticker := time.NewTicker(fleetFollowInterval)
	defer ticker.Stop()
	for {
		var addrs []string
		if s.fleetEnabled() {
			for _, ip := range s.tailnet.Addresses() {
				addrs = append(addrs, netip.AddrPortFrom(ip, s.fleetPort).String())
			}
		}
		s.updateFleetListeners(ctx, addrs)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-s.fleetChanged:
		}
	}
}

func (s *Service) updateFleetListeners(ctx context.Context, addrs []string) {
	s.fleetMut.Lock()
	defer s.fleetMut.Unlock()
	for addr, server := range s.fleetServers {
		if !slices.Contains(addrs, addr) {
			// Close, unlike Shutdown, also ends the open connections.
			server.srv.Close()
			delete(s.fleetServers, addr)
			slog.InfoContext(ctx, "Stopped agentcfg report listener", slogutil.Address(addr))
		}
	}
	for _, addr := range addrs {
		if _, ok := s.fleetServers[addr]; ok {
			continue
		}
		lst, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
		if err != nil {
			// Retried on the next round, such as once the address is
			// assigned to the interface.
			slog.WarnContext(ctx, "Failed to start agentcfg report listener", slogutil.Address(addr), slogutil.Error(err))
			continue
		}
		srv := &http.Server{
			Handler:           http.HandlerFunc(s.serveFleetRequest),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    8 << 10,
			ErrorLog:          log.New(io.Discard, "", 0),
		}
		go func() { _ = srv.Serve(lst) }()
		s.fleetServers[addr] = &fleetServer{srv: srv, addr: lst.Addr()}
		slog.InfoContext(ctx, "Serving agentcfg report on the tailnet", slogutil.Address(lst.Addr()))
	}
}

// serveFleetRequest authorizes every request with the tailnet before
// looking at it, and then serves only the report.
func (s *Service) serveFleetRequest(w http.ResponseWriter, r *http.Request) {
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err == nil {
		err = authorizeRead(r.Context(), s.tailnet, remote)
	}
	if err != nil {
		slog.DebugContext(r.Context(), "Refused agentcfg report request", slogutil.Address(r.RemoteAddr), slogutil.Error(err))
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if r.URL.Path != fleetReportPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.Status())
}

// authorizeRead accepts a node that the tailnet policy grants CapAgentcfg
// with read access. Anything else, including any error, is refused.
func authorizeRead(ctx context.Context, tn Tailnet, remote netip.AddrPort) error {
	caps, err := tn.Capabilities(ctx, remote)
	if err != nil {
		return err
	}
	grants, err := tailcfg.UnmarshalCapJSON[fleetGrant](caps, CapAgentcfg)
	if err != nil {
		return fmt.Errorf("invalid %s grant: %w", CapAgentcfg, err)
	}
	if !slices.ContainsFunc(grants, func(g fleetGrant) bool { return g.Access == "read" }) {
		return errors.New("tailnet policy does not grant read access to " + string(CapAgentcfg))
	}
	return nil
}

// FleetMember is one device in the fleet view.
type FleetMember struct {
	Name string `json:"name"`
	// Node is the tailnet node ID, empty for this device; Address is where
	// its report was read.
	Node    string  `json:"node"`
	Address string  `json:"address"`
	Status  *Status `json:"status"`
	Error   string  `json:"error"`
}

// Fleet returns the status of this device, followed by the reports of the
// tagged peers, read from their report listeners.
func (s *Service) Fleet(ctx context.Context) ([]FleetMember, error) {
	if !s.fleetEnabled() {
		return nil, errFleetDisabled
	}
	st := s.Status()
	self, _ := s.cfg.Device(s.cfg.MyID())
	nodes := s.tailnet.Nodes()
	peers := make([]FleetMember, len(nodes))
	var wg sync.WaitGroup
	for i, node := range nodes {
		wg.Go(func() { peers[i] = s.fetchReport(ctx, node) })
	}
	wg.Wait()
	slices.SortFunc(peers, func(a, b FleetMember) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Node, b.Node))
	})
	return append([]FleetMember{{Name: self.Name, Status: &st}}, peers...), nil
}

// fleetClient never uses a proxy, which would hide our tailnet address from
// the peer, and never follows a redirect, which could lead anywhere.
var fleetClient = &http.Client{
	Transport: &http.Transport{DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func (s *Service) fetchReport(ctx context.Context, node tailscale.Node) FleetMember {
	m := FleetMember{Name: node.Name, Node: node.ID}
	ctx, cancel := context.WithTimeout(ctx, fleetPeerTimeout)
	defer cancel()
	var errs []error
	for _, ip := range node.Addrs {
		addr := netip.AddrPortFrom(ip, s.fleetPort).String()
		st, answered, err := getReport(ctx, addr)
		if err == nil {
			m.Address = addr
			m.Status = &st
			return m
		}
		errs = append(errs, err)
		if answered {
			break
		}
	}
	if len(errs) == 0 {
		errs = append(errs, errors.New("no tailnet address"))
	}
	m.Error = errors.Join(errs...).Error()
	return m
}

// getReport reads a peer's report. answered says whether the peer
// responded at all, in which case its other addresses would say the same.
func getReport(ctx context.Context, addr string) (Status, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+fleetReportPath, nil)
	if err != nil {
		return Status{}, false, err
	}
	resp, err := fleetClient.Do(req)
	if err != nil {
		return Status{}, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		return Status{}, true, fmt.Errorf("%s refused the request; the tailnet policy must grant this node %s with {\"access\": \"read\"}", addr, CapAgentcfg)
	default:
		return Status{}, true, fmt.Errorf("%s answered %s", addr, resp.Status)
	}
	var st Status
	if err := json.NewDecoder(io.LimitReader(resp.Body, fleetMaxResponse)).Decode(&st); err != nil {
		return Status{}, true, fmt.Errorf("%s sent an invalid report: %w", addr, err)
	}
	return st, true, nil
}
