// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build tailscale_e2e

// Command localapiproxy forwards a Unix socket to tailscaled's LocalAPI
// socket, so that the end-to-end tests can make the LocalAPI unreachable
// while tailscaled keeps carrying traffic. SIGUSR1 closes every connection
// and refuses new ones; SIGUSR2 accepts them again.
//
// Usage: localapiproxy <listen socket> <tailscaled socket>
package main

import (
	"context"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

func main() {
	// The paths come from the test harness, which owns the container.
	listen, target := os.Args[1], os.Args[2]
	ctx := context.Background()

	var mut sync.Mutex
	conns := make(map[net.Conn]struct{})
	cut := false

	_ = os.Remove(listen) //nolint:gosec // see above
	var lc net.ListenConfig
	lst, err := lc.Listen(ctx, "unix", listen)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.Chmod(listen, 0o666); err != nil { //nolint:gosec // see above
		log.Fatal(err)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1, syscall.SIGUSR2)
	go func() {
		for sig := range signals {
			mut.Lock()
			cut = sig == syscall.SIGUSR1
			if cut {
				for c := range conns {
					c.Close()
				}
			}
			mut.Unlock()
		}
	}()

	for {
		client, err := lst.Accept()
		if err != nil {
			log.Fatal(err)
		}
		mut.Lock()
		if cut {
			mut.Unlock()
			client.Close()
			continue
		}
		var dialer net.Dialer
		server, err := dialer.DialContext(ctx, "unix", target)
		if err != nil {
			mut.Unlock()
			client.Close()
			continue
		}
		conns[client] = struct{}{}
		conns[server] = struct{}{}
		mut.Unlock()
		go func() {
			var wg sync.WaitGroup
			wg.Go(func() { _, _ = io.Copy(server, client); server.Close() })
			wg.Go(func() { _, _ = io.Copy(client, server); client.Close() })
			wg.Wait()
			mut.Lock()
			delete(conns, client)
			delete(conns, server)
			mut.Unlock()
		}()
	}
}
