// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package discover

import (
	"context"
	"time"

	"github.com/syncthing/syncthing/lib/protocol"
)

const tailscaleIdentity = "tailscale"

// A TailnetResolver resolves device IDs to the addresses of the tailnet
// peers that presented them. lib/tailscale implements it.
type TailnetResolver interface {
	DeviceAddresses(protocol.DeviceID) []string
	Devices() map[protocol.DeviceID][]string
	Error() error
}

// tailscaleFinder exposes a TailnetResolver as a Finder. The resolver is a
// service supervised elsewhere, so the finder has no service of its own.
type tailscaleFinder struct {
	tailnet TailnetResolver
}

func (f tailscaleFinder) Lookup(_ context.Context, deviceID protocol.DeviceID) ([]string, error) {
	return f.tailnet.DeviceAddresses(deviceID), nil
}

func (f tailscaleFinder) Error() error {
	return f.tailnet.Error()
}

func (tailscaleFinder) String() string {
	return tailscaleIdentity
}

// Cache returns the current tailnet table; it is live, like the local
// discovery cache.
func (f tailscaleFinder) Cache() map[protocol.DeviceID]CacheEntry {
	now := time.Now()
	devices := f.tailnet.Devices()
	res := make(map[protocol.DeviceID]CacheEntry, len(devices))
	for id, addrs := range devices {
		res[id] = CacheEntry{Addresses: addrs, when: now, found: true}
	}
	return res
}
