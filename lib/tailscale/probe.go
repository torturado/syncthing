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

	"github.com/syncthing/syncthing/lib/protocol"
)

var errProbeComplete = errors.New("identity probe complete")

// probeDeviceID returns the device ID of the BEP peer at addr.
//
// The TLS 1.3 client verifies the server's CertificateVerify and Finished
// messages before it is asked for a client certificate, so by the time
// GetClientCertificate runs the peer has proven that it holds the key of the
// certificate it presented. The probe aborts there and never sends a
// certificate, so the peer does not learn our device ID.
func probeDeviceID(ctx context.Context, tlsCfg *tls.Config, addr string) (protocol.DeviceID, error) {
	var leaf []byte
	cfg := tlsCfg.Clone()
	cfg.Certificates = nil
	// As for BEP, the device ID is the identity; there is no CA to verify.
	cfg.InsecureSkipVerify = true
	cfg.SessionTicketsDisabled = true
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) != 1 {
			return fmt.Errorf("peer presented %d certificates", len(cs.PeerCertificates))
		}
		leaf = cs.PeerCertificates[0].Raw
		return nil
	}
	cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		return nil, errProbeComplete
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return protocol.EmptyDeviceID, err
	}
	defer conn.Close()

	// Only errProbeComplete proves that the peer finished its side of the
	// handshake and asked for a client certificate, as BEP listeners do.
	err = tls.Client(conn, cfg).HandshakeContext(ctx)
	if !errors.Is(err, errProbeComplete) {
		if err == nil {
			err = errors.New("peer did not request a client certificate")
		}
		return protocol.EmptyDeviceID, err
	}
	return protocol.NewDeviceID(leaf), nil
}
