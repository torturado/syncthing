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
	"net"
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

var syncthingBinary, localAPIProxyBinary, derperBinary string

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
	localAPIProxyBinary = filepath.Join(dir, "localapiproxy")
	build = exec.Command("go", "build", "-tags", "tailscale_e2e", "-o", localAPIProxyBinary, "./localapiproxy")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building localapiproxy:", err)
		os.Exit(1)
	}
	// The DERP relay, built outside this module from the same Tailscale
	// release as the node image.
	install := exec.Command("go", "install", "tailscale.com/cmd/derper@"+strings.TrimPrefix(tailscaleImage, "tailscale/tailscale:"))
	install.Dir = dir
	install.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOBIN="+dir, "GOFLAGS=-mod=mod")
	install.Stdout, install.Stderr = os.Stderr, os.Stderr
	if err := install.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building derper:", err)
		os.Exit(1)
	}
	derperBinary = filepath.Join(dir, "derper")
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
	tailnet []*node // nodes started on the tailnet
}

// headscaleConfig is formatted with the IPv4 and IPv6 prefixes and the
// allocation mode.
const headscaleConfig = `server_url: http://headscale:8080
listen_addr: 0.0.0.0:8080
metrics_listen_addr: 127.0.0.1:9090
grpc_listen_addr: 127.0.0.1:50443
noise:
  private_key_path: /var/lib/headscale/noise_private.key
prefixes:
  v4: %s
  v6: %s
  allocation: %s
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

// The tailnet has its own DERP relay, as every real tailnet does, so that
// nodes can talk while their direct paths are set up. It serves plain HTTP,
// which the clients accept for nodes marked forcehttp.
const derpMap = `regions:
  900:
    regionid: 900
    regioncode: test
    regionname: Test DERP
    nodes:
      - name: 900a
        regionid: 900
        hostname: derper
        derpport: 3340
        stunport: 3478
        forcehttp: true
`

// nodeScript starts tailscaled with a kernel TUN device, joins the tailnet
// and runs Syncthing in the foreground. Without an auth key the node stays
// off the tailnet and is only reachable on the Docker network. Syncthing
// reaches the LocalAPI through localapiproxy, which cutLocalAPI uses.
const nodeScript = `set -e
mkdir -p /var/lib/tailscale /data
# A seeded identity replaces the one Syncthing would generate.
if [ -d /seed ]; then cp /seed/cert.pem /seed/key.pem /data/; fi
if [ -n "$TS_AUTHKEY" ]; then
  ` + tailscaledCommand + ` &
  i=0; until [ -S /var/run/tailscale/real.sock ] || [ $i -gt 100 ]; do i=$((i+1)); sleep 0.1; done
  /usr/local/bin/localapiproxy /var/run/tailscale/tailscaled.sock /var/run/tailscale/real.sock >>/var/log/localapiproxy.log 2>&1 &
  i=0; until [ -S /var/run/tailscale/tailscaled.sock ] || [ $i -gt 100 ]; do i=$((i+1)); sleep 0.1; done
  tailscale up --login-server=http://headscale:8080 --authkey="$TS_AUTHKEY" --hostname="$NODE_NAME" --accept-dns=false
fi
exec /usr/local/bin/syncthing serve --home=/data --no-browser --no-upgrade --no-restart --gui-address=0.0.0.0:8384 --gui-apikey=` + apiKey + ` >>/var/log/syncthing.log 2>&1
`

// tailscaledCommand runs tailscaled on the real LocalAPI socket.
const tailscaledCommand = `TS_DEBUG_FIREWALL_MODE=nftables tailscaled --state=/var/lib/tailscale/tailscaled.state --socket=/var/run/tailscale/real.sock >>/var/log/tailscaled.log 2>&1`

func newHarness(t *testing.T, policy string) *harness {
	t.Helper()
	return newHarnessWithPrefixes(t, policy, "100.64.0.0/10", "fd7a:115c:a1e0::/48", "sequential")
}

// newHarnessWithPrefixes is newHarness with the tailnet's address prefixes
// and Headscale's allocation mode ("sequential" or "random").
func newHarnessWithPrefixes(t *testing.T, policy, v4, v6, allocation string) *harness {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required")
	}
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	h := &harness{t: t, prefix: "st-tailnet-" + hex.EncodeToString(suffix), dir: t.TempDir()}
	h.network = h.prefix
	h.control = h.prefix + "-headscale"

	h.write("config.yaml", fmt.Sprintf(headscaleConfig, v4, v6, allocation))
	h.write("derp.yaml", derpMap)
	h.write("policy.json", policy)
	h.write("node.sh", nodeScript)

	h.docker("network", "create", h.network)
	t.Cleanup(h.cleanup)
	h.docker("run", "-d", "--name", h.prefix+"-derper", "--network", h.network, "--network-alias", "derper",
		"-v", derperBinary+":/usr/local/bin/derper:ro",
		"--entrypoint", "/usr/local/bin/derper", tailscaleImage, "-a", ":3340", "-http-port", "-1", "-stun-port", "3478")
	h.docker("run", "-d", "--name", h.control, "--network", h.network, "--network-alias", "headscale",
		"-v", filepath.Join(h.dir, "config.yaml")+":/etc/headscale/config.yaml:ro",
		"-v", filepath.Join(h.dir, "derp.yaml")+":/etc/headscale/derp.yaml:ro",
		"-v", h.dir+":/policy:ro",
		headscaleImage, "serve", "-c", "/etc/headscale/config.yaml")
	// The policy is read through the directory mount so that setPolicy can
	// replace the file.
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

// setPolicy replaces the tailnet policy and makes Headscale reload it.
// Headscale keeps the old policy if the new one is invalid, so the new one
// is checked first.
func (h *harness) setPolicy(policy string) {
	h.t.Helper()
	h.write("policy.next.json", policy)
	h.docker("exec", h.control, "headscale", "policy", "check", "-f", "/policy/policy.next.json")
	h.write("policy.json", policy)
	h.docker("kill", "-s", "HUP", h.control)
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
	h          *harness
	name       string
	container  string
	api        string
	id         protocol.DeviceID
	offTailnet bool // expelled, expired or without tailscaled
}

func (h *harness) startNode(name, authKey string) *node {
	h.t.Helper()
	return h.startNodeSeeded(name, authKey, "")
}

// startNodeWithIdentity starts a node whose Syncthing uses the certificate
// and key of another node.
func (h *harness) startNodeWithIdentity(name, authKey string, from *node) *node {
	h.t.Helper()
	seed := filepath.Join(h.dir, "seed-"+name)
	if err := os.MkdirAll(seed, 0o755); err != nil {
		h.t.Fatal(err)
	}
	for _, f := range []string{"cert.pem", "key.pem"} {
		h.docker("cp", from.container+":/data/"+f, filepath.Join(seed, f))
	}
	return h.startNodeSeeded(name, authKey, seed)
}

func (h *harness) startNodeSeeded(name, authKey, seed string) *node {
	h.t.Helper()
	n := &node{h: h, name: name, container: h.prefix + "-" + name}
	args := []string{"run", "-d", "--name", n.container, "--hostname", name, "--network", h.network,
		"--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW", "--device", "/dev/net/tun",
		"-e", "TS_AUTHKEY=" + authKey, "-e", "NODE_NAME=" + name,
		"-v", syncthingBinary + ":/usr/local/bin/syncthing:ro",
		"-v", localAPIProxyBinary + ":/usr/local/bin/localapiproxy:ro",
		"-v", filepath.Join(h.dir, "node.sh") + ":/node.sh:ro",
		"-p", "127.0.0.1::8384"}
	if seed != "" {
		args = append(args, "-v", seed+":/seed:ro")
	}
	h.docker(append(args, "--entrypoint", "sh", tailscaleImage, "/node.sh")...)
	h.t.Cleanup(func() {
		if h.t.Failed() {
			out, _ := h.dockerErr("exec", n.container, "sh", "-c", "tail -n 200 /var/log/syncthing.log; echo ---; tail -n 30 /var/log/tailscaled.log")
			h.t.Logf("%s logs:\n%s", name, out)
		}
	})
	n.api = h.docker("port", n.container, "8384/tcp")

	// A new node reaches the others only once direct paths are set up and
	// Headscale has pushed packet filters that include it, which can take a
	// while; wait for TCP to the BEP port both ways, so that the tests
	// measure Syncthing rather than the control plane.
	if authKey != "" {
		for _, other := range h.tailnet {
			if other.offTailnet {
				continue
			}
			waitFor(h.t, name+" and "+other.name+" to reach each other over the tailnet", 3*time.Minute, func() bool {
				return n.reachesBEP(other) && other.reachesBEP(n)
			})
		}
		h.tailnet = append(h.tailnet, n)
	}

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
		// Auto-accepted folders land in /data/<label>.
		cfg.Defaults.Folder.Path = "/data"
		cfg.Defaults.Folder.RescanIntervalS = 5
	})
	return n
}

// lanIP returns the node's address on the Docker network.
func (n *node) lanIP() string {
	n.h.t.Helper()
	return n.h.docker("inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", n.container)
}

// deleteNode removes the node from the tailnet, as an admin expelling it.
func (h *harness) deleteNode(n *node) {
	h.t.Helper()
	n.offTailnet = true
	h.docker("exec", h.control, "headscale", "nodes", "delete", "-i", h.nodeID(n), "--force")
}

// expireNode expires the node's key, forcing it to authenticate again.
func (h *harness) expireNode(n *node) {
	h.t.Helper()
	n.offTailnet = true
	h.docker("exec", h.control, "headscale", "nodes", "expire", "-i", h.nodeID(n))
}

func (h *harness) nodeID(n *node) string {
	h.t.Helper()
	var nodes []struct {
		ID   uint64 `json:"id"`
		Name string `json:"name"`
	}
	h.jsonOut(&nodes, "exec", h.control, "headscale", "nodes", "list", "-o", "json")
	for _, hn := range nodes {
		if hn.Name == n.name {
			return fmt.Sprint(hn.ID)
		}
	}
	h.t.Fatalf("node %s not found in headscale", n.name)
	return ""
}

func (n *node) config() config.Configuration {
	n.h.t.Helper()
	var cfg config.Configuration
	n.rest(http.MethodGet, "/rest/config", nil, &cfg)
	return cfg
}

func (n *node) pendingDevices() map[string]any {
	n.h.t.Helper()
	var pending map[string]any
	n.rest(http.MethodGet, "/rest/cluster/pending/devices", nil, &pending)
	return pending
}

// completion returns how complete peer is on folder, as this node sees it.
func (n *node) completion(folder string, peer *node) float64 {
	n.h.t.Helper()
	var res struct {
		Completion float64 `json:"completion"`
	}
	n.rest(http.MethodGet, "/rest/db/completion?folder="+folder+"&device="+peer.id.String(), nil, &res)
	return res.Completion
}

func (n *node) exec(args ...string) string {
	n.h.t.Helper()
	return n.h.docker(append([]string{"exec", n.container}, args...)...)
}

// tailnetIPs returns the node's tailnet addresses, IPv4 first. It asks
// tailscaled directly, so it works while the LocalAPI is cut.
func (n *node) tailnetIPs() []string {
	n.h.t.Helper()
	return strings.Fields(n.exec("tailscale", "--socket=/var/run/tailscale/real.sock", "ip"))
}

// reachesBEP reports whether the node can open a TCP connection to the
// other node's BEP port over the tailnet.
func (n *node) reachesBEP(other *node) bool {
	addr := net.JoinHostPort(other.tailnetIPs()[0], "22000")
	_, err := n.h.dockerErr("exec", n.container, "sh", "-c", "nc -w 2 "+addr+" </dev/null")
	return err == nil
}

// stopTailscaled stops the node's tailscaled; restartTailscaled starts it
// again with the same state.
func (n *node) stopTailscaled() {
	n.h.t.Helper()
	n.offTailnet = true
	n.exec("pkill", "tailscaled")
}

func (n *node) restartTailscaled() {
	n.h.t.Helper()
	n.offTailnet = false
	n.exec("sh", "-c", tailscaledCommand+" &")
}

// cutLocalAPI makes the LocalAPI unreachable for Syncthing while tailscaled
// keeps running; restoreLocalAPI undoes it.
func (n *node) cutLocalAPI() {
	n.h.t.Helper()
	n.exec("pkill", "-USR1", "localapiproxy")
}

func (n *node) restoreLocalAPI() {
	n.h.t.Helper()
	n.exec("pkill", "-USR2", "localapiproxy")
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
