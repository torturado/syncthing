// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build tailscale_e2e

// Package tailscale runs Syncthing on a real tailnet: Headscale as the
// control server and one Docker container per node, each running tailscaled
// with a kernel TUN device and the Syncthing binary built from this tree.
package tailscale

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
)

const (
	headscaleImage = "headscale/headscale:0.29.7"
	tailscaleImage = "tailscale/tailscale:v1.104.7"
	apiKey         = "tailnet-e2e"
)

var syncthingBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "syncthing-tailnet-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	syncthingBinary = filepath.Join(dir, "syncthing")
	build := exec.Command("go", "run", "build.go", "-no-upgrade", "-build-out", syncthingBinary, "build", "syncthing")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building syncthing:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// harness owns one tailnet: a Docker network, a Headscale container and the
// node containers started on it. Everything is removed when the test ends.
type harness struct {
	t       *testing.T
	prefix  string
	dir     string
	network string
	control string
	userID  string
}

const headscaleConfig = `server_url: http://headscale:8080
listen_addr: 0.0.0.0:8080
metrics_listen_addr: 127.0.0.1:9090
grpc_listen_addr: 127.0.0.1:50443
noise:
  private_key_path: /var/lib/headscale/noise_private.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
  allocation: sequential
derp:
  server:
    enabled: false
  urls: []
  paths: [/etc/headscale/derp.yaml]
  auto_update_enabled: false
database:
  type: sqlite
  sqlite:
    path: /var/lib/headscale/db.sqlite
policy:
  mode: file
  path: /policy/policy.json
dns:
  magic_dns: true
  base_domain: tailnet.test
  override_local_dns: false
`

// Nodes share one Docker bridge and reach each other directly, so the DERP
// map only has to satisfy Headscale's requirement for one region.
const derpMap = `regions:
  900:
    regionid: 900
    regioncode: none
    regionname: Unreachable placeholder
    nodes:
      - name: 900a
        regionid: 900
        hostname: derp.invalid
        stunport: -1
`

// nodeScript starts tailscaled with a kernel TUN device, joins the tailnet
// and runs Syncthing in the foreground.
const nodeScript = `set -e
mkdir -p /var/lib/tailscale /data
tailscaled --state=/var/lib/tailscale/tailscaled.state >>/var/log/tailscaled.log 2>&1 &
i=0; until [ -S /var/run/tailscale/tailscaled.sock ] || [ $i -gt 100 ]; do i=$((i+1)); sleep 0.1; done
tailscale up --login-server=http://headscale:8080 --authkey="$TS_AUTHKEY" --hostname="$NODE_NAME" --accept-dns=false
exec /usr/local/bin/syncthing serve --home=/data --no-browser --no-upgrade --no-restart --gui-address=0.0.0.0:8384 --gui-apikey=` + apiKey + ` >>/var/log/syncthing.log 2>&1
`

func newHarness(t *testing.T, policy string) *harness {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required")
	}
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	h := &harness{t: t, prefix: "st-tailnet-" + hex.EncodeToString(suffix), dir: t.TempDir()}
	h.network = h.prefix
	h.control = h.prefix + "-headscale"

	h.write("config.yaml", headscaleConfig)
	h.write("derp.yaml", derpMap)
	h.write("policy.json", policy)
	h.write("node.sh", nodeScript)

	h.docker("network", "create", h.network)
	t.Cleanup(h.cleanup)
	h.docker("run", "-d", "--name", h.control, "--network", h.network, "--network-alias", "headscale",
		"-v", filepath.Join(h.dir, "config.yaml")+":/etc/headscale/config.yaml:ro",
		"-v", filepath.Join(h.dir, "derp.yaml")+":/etc/headscale/derp.yaml:ro",
		"-v", h.dir+":/policy:ro",
		headscaleImage, "serve", "-c", "/etc/headscale/config.yaml")
	waitFor(t, "headscale", 30*time.Second, func() bool {
		_, err := h.dockerErr("exec", h.control, "headscale", "users", "list")
		return err == nil
	})
	h.docker("exec", h.control, "headscale", "users", "create", "syncthing")
	var users []struct {
		ID uint64 `json:"id"`
	}
	h.jsonOut(&users, "exec", h.control, "headscale", "users", "list", "-o", "json")
	h.userID = fmt.Sprint(users[0].ID)
	return h
}

func (h *harness) write(name, content string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, name), []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) cleanup() {
	if h.t.Failed() {
		out, _ := h.dockerErr("logs", "--tail", "50", h.control)
		h.t.Logf("headscale log:\n%s", out)
	}
	ids, _ := h.dockerErr("ps", "-aq", "--filter", "name="+h.prefix)
	if ids != "" {
		_, _ = h.dockerErr(append([]string{"rm", "-f"}, strings.Fields(ids)...)...)
	}
	_, _ = h.dockerErr("network", "rm", h.network)
}

func (h *harness) dockerErr(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

func (h *harness) docker(args ...string) string {
	h.t.Helper()
	out, err := h.dockerErr(args...)
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *harness) jsonOut(v any, args ...string) {
	h.t.Helper()
	if err := json.Unmarshal([]byte(h.docker(args...)), v); err != nil {
		h.t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
	}
}

// authKey returns a pre-authentication key; nodes that use it are tagged
// with the given tags, or owned by the user when there are none.
func (h *harness) authKey(tags ...string) string {
	h.t.Helper()
	args := []string{"exec", h.control, "headscale", "preauthkeys", "create", "-u", h.userID, "--reusable", "-o", "json"}
	if len(tags) > 0 {
		args = append(args, "--tags", strings.Join(tags, ","))
	}
	var key struct {
		Key string `json:"key"`
	}
	h.jsonOut(&key, args...)
	return key.Key
}

// node is one tailnet node running tailscaled and Syncthing.
type node struct {
	h         *harness
	name      string
	container string
	api       string
	id        protocol.DeviceID
}

func (h *harness) startNode(name, authKey string) *node {
	h.t.Helper()
	n := &node{h: h, name: name, container: h.prefix + "-" + name}
	h.docker("run", "-d", "--name", n.container, "--hostname", name, "--network", h.network,
		"--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW", "--device", "/dev/net/tun",
		"-e", "TS_DEBUG_FIREWALL_MODE=nftables", "-e", "TS_AUTHKEY="+authKey, "-e", "NODE_NAME="+name,
		"-v", syncthingBinary+":/usr/local/bin/syncthing:ro",
		"-v", filepath.Join(h.dir, "node.sh")+":/node.sh:ro",
		"-p", "127.0.0.1::8384",
		"--entrypoint", "sh", tailscaleImage, "/node.sh")
	h.t.Cleanup(func() {
		if h.t.Failed() {
			out, _ := h.dockerErr("exec", n.container, "sh", "-c", "tail -n 80 /var/log/syncthing.log; echo ---; tail -n 30 /var/log/tailscaled.log")
			h.t.Logf("%s logs:\n%s", name, out)
		}
	})
	n.api = h.docker("port", n.container, "8384/tcp")

	var status struct {
		MyID protocol.DeviceID `json:"myID"`
	}
	waitFor(h.t, name+" Syncthing API", 60*time.Second, func() bool {
		return n.restErr(http.MethodGet, "/rest/system/status", nil, &status) == nil
	})
	n.id = status.MyID

	// Only the tailnet may find peers: no global or local discovery, no
	// relays, no NAT traversal, and nothing that phones home.
	n.modifyConfig(func(cfg *config.Configuration) {
		cfg.Folders = nil
		cfg.Options.GlobalAnnEnabled = false
		cfg.Options.LocalAnnEnabled = false
		cfg.Options.RelaysEnabled = false
		cfg.Options.NATEnabled = false
		cfg.Options.URAccepted = -1
		cfg.Options.CREnabled = false
		cfg.Options.AutoUpgradeIntervalH = 0
		cfg.Options.RawListenAddresses = []string{"tcp://0.0.0.0:22000"}
		cfg.Options.ReconnectIntervalS = 5
	})
	return n
}

func (n *node) exec(args ...string) string {
	n.h.t.Helper()
	return n.h.docker(append([]string{"exec", n.container}, args...)...)
}

// tailnetIPs returns the node's tailnet addresses, IPv4 first.
func (n *node) tailnetIPs() []string {
	n.h.t.Helper()
	return strings.Fields(n.exec("tailscale", "ip"))
}

func (n *node) restErr(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		bs, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(bs)
	}
	req, err := http.NewRequest(method, "http://"+n.api+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	bs, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bs)
	}
	if out != nil {
		return json.Unmarshal(bs, out)
	}
	return nil
}

func (n *node) rest(method, path string, body, out any) {
	n.h.t.Helper()
	if err := n.restErr(method, path, body, out); err != nil {
		n.h.t.Fatalf("%s: %v", n.name, err)
	}
}

func (n *node) modifyConfig(mutate func(*config.Configuration)) {
	n.h.t.Helper()
	var cfg config.Configuration
	n.rest(http.MethodGet, "/rest/config", nil, &cfg)
	mutate(&cfg)
	n.rest(http.MethodPut, "/rest/config", cfg, nil)
}

// addDevice configures a peer by device ID only; its addresses must come
// from discovery.
func (n *node) addDevice(peer *node) {
	n.h.t.Helper()
	n.modifyConfig(func(cfg *config.Configuration) {
		dev := cfg.Defaults.Device.Copy()
		dev.DeviceID = peer.id
		dev.Name = peer.name
		dev.Addresses = []string{"dynamic"}
		cfg.Devices = append(cfg.Devices, dev)
	})
}

// addFolder creates folder id at /data/<id>, shared with the given peers.
func (n *node) addFolder(id string, peers ...*node) {
	n.h.t.Helper()
	n.exec("mkdir", "-p", "/data/"+id)
	n.modifyConfig(func(cfg *config.Configuration) {
		fcfg := cfg.Defaults.Folder.Copy()
		fcfg.ID = id
		fcfg.Label = id
		fcfg.Path = "/data/" + id
		fcfg.RescanIntervalS = 5
		for _, p := range peers {
			fcfg.Devices = append(fcfg.Devices, config.FolderDeviceConfiguration{DeviceID: p.id})
		}
		cfg.Folders = append(cfg.Folders, fcfg)
	})
}

type connection struct {
	Connected bool   `json:"connected"`
	Address   string `json:"address"`
}

func (n *node) connections() map[string]connection {
	n.h.t.Helper()
	var res struct {
		Connections map[string]connection `json:"connections"`
	}
	n.rest(http.MethodGet, "/rest/system/connections", nil, &res)
	return res.Connections
}

func (n *node) connectedTo(peer *node) (connection, bool) {
	n.h.t.Helper()
	c, ok := n.connections()[peer.id.String()]
	return c, ok && c.Connected
}

func (n *node) fileContent(path string) (string, bool) {
	out, err := n.h.dockerErr("exec", n.container, "cat", path)
	return out, err == nil
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
