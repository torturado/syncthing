// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package dialer

import (
	"net"
	"testing"
)

type peerConn struct {
	net.Conn
	remote net.Addr
}

func (c peerConn) RemoteAddr() net.Addr { return c.remote }

func TestIsProxied(t *testing.T) {
	peer := &net.TCPAddr{IP: net.ParseIP("100.64.0.2"), Port: 22000}
	proxy := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1080}
	requested := newDialerAddr("tcp", "100.64.0.2:22000")

	if IsProxied(peerConn{remote: peer}) {
		t.Error("a plain connection counts as proxied")
	}
	// The proxy dialer wraps connections that no_proxy sent directly, too.
	if IsProxied(dialerConn{peerConn{remote: peer}, requested}) {
		t.Error("a direct connection from the proxy dialer counts as proxied")
	}
	if !IsProxied(dialerConn{peerConn{remote: proxy}, requested}) {
		t.Error("a connection through the proxy does not count as proxied")
	}
}
