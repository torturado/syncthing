// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package agentcfg

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
)

// testSpec declares one server that the empty test home lacks.
const testSpec = `[mcp_servers.search]
url = "https://search.example.com/mcp"
agents = ["claude"]
`

type testEnv struct {
	svc      *Service
	cfg      config.Wrapper
	evLogger events.Logger
	home     string
	folder   string // root of the shared folder "agents"
	runs     atomic.Int32
	scans    scanRecorder
}

// newTestEnv returns a service over a temporary home with an empty Claude
// configuration and one shared folder, configured by mutate. Nothing outside
// the temporary directories is read or written.
func newTestEnv(t *testing.T, mutate func(*config.Configuration)) *testEnv {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	env := &testEnv{home: t.TempDir(), folder: t.TempDir()}
	writeFile(t, filepath.Join(env.home, ".claude.json"), "{}")

	cfg := config.New(protocol.LocalDeviceID)
	cfg.Folders = []config.FolderConfiguration{{ID: "agents", Path: env.folder, FilesystemType: config.FilesystemTypeBasic}}
	cfg.Options.AgentcfgEnabled = true
	// The fleet view is on by default; the tests that need it turn it on.
	cfg.Options.AgentcfgFleetEnabled = false
	cfg.Options.AgentcfgIntervalS = 3600
	if mutate != nil {
		mutate(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	env.evLogger = events.NewLogger()
	go env.evLogger.Serve(ctx)
	env.cfg = config.Wrap("", cfg, protocol.LocalDeviceID, env.evLogger)
	go env.cfg.Serve(ctx)

	env.svc = New(env.cfg, env.evLogger, &env.scans)
	env.svc.home = env.home
	// Counts the runs, and keeps the tailscale CLI out of the tests.
	env.svc.hostNames = func(context.Context) []string {
		env.runs.Add(1)
		return []string{"test-host"}
	}
	return env
}

func (env *testEnv) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := env.svc.ServeBackground(ctx)
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Replace the file by rename, as agents do.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	bs, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Syncthing writes .stignore with CRLF line endings on Windows.
	return strings.ReplaceAll(string(bs), "\r\n", "\n")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func reportErrors(st Status) int {
	if st.Report == nil {
		return -1
	}
	return st.Report.Errors
}

func TestFindSpec(t *testing.T) {
	a, b, enc := t.TempDir(), t.TempDir(), t.TempDir()
	cfg := config.New(protocol.LocalDeviceID)
	cfg.Folders = []config.FolderConfiguration{
		{ID: "a", Path: a, FilesystemType: config.FilesystemTypeBasic},
		{ID: "b", Path: b, FilesystemType: config.FilesystemTypeBasic},
		{ID: "enc", Path: enc, FilesystemType: config.FilesystemTypeBasic, Type: config.FolderTypeReceiveEncrypted},
	}

	if _, err := findSpec(cfg); err == nil {
		t.Error("found a spec in folders that hold none")
	}

	// A receive-encrypted folder is never read.
	writeFile(t, filepath.Join(enc, SpecFileName), testSpec)
	if _, err := findSpec(cfg); err == nil {
		t.Error("found the spec of a receive-encrypted folder")
	}

	writeFile(t, filepath.Join(a, SpecFileName), testSpec)
	got, err := findSpec(cfg)
	if err != nil || got.path != filepath.Join(a, SpecFileName) || got.folder != "a" || got.explicit {
		t.Errorf("findSpec with one spec = %+v, %v; want the spec of folder a, found", got, err)
	}

	writeFile(t, filepath.Join(b, SpecFileName), testSpec)
	if _, err := findSpec(cfg); err == nil || !strings.Contains(err.Error(), "a, b") {
		t.Errorf("findSpec with two specs: %v; want an error naming both folders", err)
	}

	cfg.Options.AgentcfgSpecPath = filepath.Join(b, "sub", "spec.toml")
	got, err = findSpec(cfg)
	if err != nil || got.folder != "b" || got.name != filepath.Join("sub", "spec.toml") || !got.explicit {
		t.Errorf("findSpec with a configured path = %+v, %v; want sub/spec.toml in folder b, explicit", got, err)
	}
}

func TestSpecTargetConcerns(t *testing.T) {
	target := specTarget{path: "/f/agentcfg.toml", folder: "agents", name: SpecFileName}
	cases := []struct {
		ev   events.Event
		want bool
	}{
		{events.Event{Type: events.LocalIndexUpdated, Data: map[string]any{"folder": "agents", "filenames": []string{"notes.md", SpecFileName}}}, true},
		{events.Event{Type: events.ItemFinished, Data: map[string]any{"folder": "agents", "item": SpecFileName}}, true},
		{events.Event{Type: events.ItemFinished, Data: map[string]any{"folder": "agents", "item": "notes.md"}}, false},
		{events.Event{Type: events.ItemFinished, Data: map[string]any{"folder": "other", "item": SpecFileName}}, false},
	}
	for _, c := range cases {
		if got := target.concerns(c.ev); got != c.want {
			t.Errorf("concerns(%v %v) = %v, want %v", c.ev.Type, c.ev.Data, got, c.want)
		}
	}
	// While no spec is found, a spec appearing in any folder concerns it.
	ev := events.Event{Type: events.ItemFinished, Data: map[string]any{"folder": "other", "item": SpecFileName}}
	if !(specTarget{}).concerns(ev) {
		t.Error("a new spec in a folder does not concern a service without one")
	}
}

func TestChecksFollowSpecAndAgentFiles(t *testing.T) {
	env := newTestEnv(t, nil)
	sub := env.evLogger.Subscribe(events.AgentConfigChanged)
	defer sub.Unsubscribe()
	spec := filepath.Join(env.folder, SpecFileName)

	// No spec in the folder yet: nothing to check, and nothing wrong.
	env.start(t)
	waitFor(t, "the first check", func() bool {
		env.svc.runMut.Lock()
		defer env.svc.runMut.Unlock()
		return env.svc.stamps != nil
	})
	if st := env.svc.Status(); st.Error != "" || st.Report != nil {
		t.Errorf("status without a spec = %+v; want neither an error nor a report", st)
	}

	// Syncthing pulls a spec into the folder.
	writeFile(t, spec, testSpec)
	env.evLogger.Log(events.ItemFinished, map[string]any{"folder": "agents", "item": SpecFileName, "action": "update"})
	waitFor(t, "the drift to be reported", func() bool { return reportErrors(env.svc.Status()) == 1 })
	if st := env.svc.Status(); st.Spec != spec || st.Folder != "agents" || st.Report.Findings[0].Rule != "missing-server" {
		t.Errorf("status = %+v, want a missing-server finding against the folder's spec", st)
	}
	if got := readFile(t, filepath.Join(env.home, ".claude.json")); got != "{}" {
		t.Errorf("check wrote %q with automatic apply off", got)
	}

	// Another tool adds the server.
	writeFile(t, filepath.Join(env.home, ".claude.json"), `{"mcpServers": {"search": {"type": "http", "url": "https://search.example.com/mcp"}}}`)
	waitFor(t, "the fix to be reported", func() bool { return reportErrors(env.svc.Status()) == 0 })

	var changes []map[string]any
	for len(sub.C()) > 0 {
		ev := <-sub.C()
		changes = append(changes, ev.Data.(map[string]any))
	}
	if len(changes) != 2 || changes[0]["errors"] != 1 || changes[1]["ok"] != true {
		t.Errorf("AgentConfigChanged events = %v; want one error, then ok", changes)
	}
}

func TestAutoApplyDoesNotTriggerItself(t *testing.T) {
	var spec string
	env := newTestEnv(t, func(cfg *config.Configuration) {
		cfg.Options.AgentcfgApply = config.AgentcfgApplyAuto
		spec = filepath.Join(cfg.Folders[0].Path, "spec.toml")
		cfg.Options.AgentcfgSpecPath = spec
	})
	writeFile(t, spec, testSpec)
	sub := env.evLogger.Subscribe(events.AgentConfigChanged)
	defer sub.Unsubscribe()
	claudeJSON := filepath.Join(env.home, ".claude.json")

	env.start(t)
	waitFor(t, "the spec to be applied", func() bool {
		st := env.svc.Status()
		return reportErrors(st) == 0 && st.LastApply != nil && st.LastApply.Written
	})
	if !strings.Contains(readFile(t, claudeJSON), "search.example.com") {
		t.Fatal("the server was not written")
	}

	// The write is seen by the file watch, but changes nothing since the
	// run that made it, so it does not run again.
	time.Sleep(debounceQuiet + time.Second)
	if n := env.runs.Load(); n != 1 {
		t.Errorf("%d runs after applying once, want 1", n)
	}

	// Another tool drops the server; it is put back, once.
	firstApply := env.svc.Status().LastApply.AppliedAt
	writeFile(t, claudeJSON, "{}")
	waitFor(t, "the drift to be corrected", func() bool {
		st := env.svc.Status()
		return st.LastApply.AppliedAt.After(firstApply) && reportErrors(st) == 0
	})
	time.Sleep(debounceQuiet + time.Second)
	if n := env.runs.Load(); n != 2 {
		t.Errorf("%d runs after correcting one drift, want 2", n)
	}
	if !strings.Contains(readFile(t, claudeJSON), "search.example.com") {
		t.Error("the server was not written back")
	}

	// The report never showed the drift, so it never changed.
	if n := len(sub.C()); n != 1 {
		t.Errorf("%d AgentConfigChanged events, want 1", n)
	}
}

func TestAutoApplyNeedsAKnownSpec(t *testing.T) {
	// Automatic apply is on by default.
	env := newTestEnv(t, nil)
	writeFile(t, filepath.Join(env.folder, SpecFileName), testSpec)
	claudeJSON := filepath.Join(env.home, ".claude.json")

	env.start(t)
	waitFor(t, "the check", func() bool { return env.svc.Status().Report != nil })
	st := env.svc.Status()
	if st.Report.Errors != 1 || st.LastApply != nil || !strings.Contains(st.ApplyError, "agentcfgSpecPath") {
		t.Errorf("status = %+v; want the drift reported, nothing applied, and why", st)
	}
	if got := readFile(t, claudeJSON); got != "{}" {
		t.Errorf("a spec found in another folder was applied: %q", got)
	}

	// The spec in ~/.agents, which the service shares itself, is applied.
	if err := os.Remove(filepath.Join(env.folder, SpecFileName)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(env.home, ".agents", SpecFileName), testSpec)
	env.markPeer(t, true)
	waitFor(t, "the spec in ~/.agents to be applied", func() bool {
		st := env.svc.Status()
		return st.Folder == agentsFolderID && st.LastApply != nil && st.LastApply.Written && reportErrors(st) == 0
	})
	if !strings.Contains(readFile(t, claudeJSON), "search.example.com") {
		t.Error("the server was not written")
	}
}

func TestDisablingStopsChecks(t *testing.T) {
	env := newTestEnv(t, nil)
	writeFile(t, filepath.Join(env.folder, SpecFileName), testSpec)
	env.start(t)
	waitFor(t, "the check", func() bool { return env.svc.Status().Report != nil })

	w, err := env.cfg.Modify(func(cfg *config.Configuration) { cfg.Options.AgentcfgEnabled = false })
	if err != nil {
		t.Fatal(err)
	}
	w.Wait()
	waitFor(t, "the status to be cleared", func() bool { return env.svc.Status().Report == nil })
	runs := env.runs.Load()
	writeFile(t, filepath.Join(env.home, ".claude.json"), `{"mcpServers": {}}`)
	time.Sleep(debounceQuiet + time.Second)
	if n := env.runs.Load(); n != runs {
		t.Errorf("%d runs while disabled", n-runs)
	}
}

// keptFolders returns the folders of agent configuration in the
// configuration, by ID.
func (env *testEnv) keptFolders() map[string]config.FolderConfiguration {
	folders := map[string]config.FolderConfiguration{}
	for _, f := range env.cfg.FolderList() {
		if strings.HasPrefix(f.ID, SyncFolderPrefix) {
			folders[f.ID] = f
		}
	}
	return folders
}

func (env *testEnv) markPeer(t *testing.T, sync bool) {
	t.Helper()
	w, err := env.cfg.Modify(func(cfg *config.Configuration) {
		cfg.SetDevice(config.DeviceConfiguration{DeviceID: peerID, Name: "peer", AgentcfgSync: sync})
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Wait()
}

func TestAgentFoldersAreSharedWithTheMarkedDevices(t *testing.T) {
	env := newTestEnv(t, func(cfg *config.Configuration) {
		// An unmarked device shares nothing.
		cfg.Devices = append(cfg.Devices, config.DeviceConfiguration{DeviceID: brokenID, Name: "other"})
	})
	writeFile(t, filepath.Join(env.home, ".claude", "settings.json"), "{}")
	writeFile(t, filepath.Join(env.folder, SpecFileName), testSpec+"\n[sync.exclude]\nagents = [\"/skills/local\"]\n")
	env.start(t)
	waitFor(t, "the first check", func() bool { return env.svc.Status().Report != nil })
	if kept := env.keptFolders(); len(kept) != 0 {
		t.Fatalf("folders %v kept while no device is marked", kept)
	}

	env.markPeer(t, true)
	waitFor(t, "the four folders", func() bool { return len(env.keptFolders()) == 4 })
	kept := env.keptFolders()
	for id, path := range map[string]string{"agentcfg-agents": ".agents", "agentcfg-claude": ".claude", "agentcfg-codex": ".codex", "agentcfg-gemini": ".gemini"} {
		f := kept[id]
		if f.Path != filepath.Join(env.home, path) || f.Type != config.FolderTypeSendReceive || !slices.Equal(f.DeviceIDs(), []protocol.DeviceID{peerID, protocol.LocalDeviceID}) {
			t.Errorf("%s = %s at %s with %v; want send-receive at ~/%s with this device and the peer", id, f.Type, f.Path, f.DeviceIDs(), path)
		}
	}
	// Only the configuration in an agent's own directory is shared, and the
	// spec's exclusions come first.
	claude := readFile(t, filepath.Join(env.home, ".claude", ".stignore"))
	if !strings.Contains(claude, "!/settings.json\n") || !strings.HasSuffix(claude, "\n/*\n") {
		t.Errorf("~/.claude/.stignore = %q; want settings.json and nothing else", claude)
	}
	if agents := readFile(t, filepath.Join(env.home, ".agents", ".stignore")); !strings.Contains(agents, "/skills/local\n**/.git") {
		t.Errorf("~/.agents/.stignore = %q; want the spec's exclusion first", agents)
	}

	// A new exclusion reaches the folder, which rescans.
	writeFile(t, filepath.Join(env.folder, SpecFileName), testSpec+"\n[sync.exclude]\nagents = [\"/skills/local\", \"/notes\"]\n")
	env.evLogger.Log(events.ItemFinished, map[string]any{"folder": "agents", "item": SpecFileName, "action": "update"})
	waitFor(t, "the new exclusion", func() bool {
		return strings.Contains(readFile(t, filepath.Join(env.home, ".agents", ".stignore")), "/notes\n") && env.scans.count("agentcfg-agents", "") > 0
	})

	// Unmarking the device stops the sharing and leaves the files.
	env.markPeer(t, false)
	waitFor(t, "the folders to go", func() bool { return len(env.keptFolders()) == 0 })
	if got := readFile(t, filepath.Join(env.home, ".claude", "settings.json")); got != "{}" {
		t.Errorf("settings.json = %q after the folder went; want it untouched", got)
	}
}

func TestAgentFolderTakenByAnotherFolderIsReported(t *testing.T) {
	var codex string
	env := newTestEnv(t, func(cfg *config.Configuration) {
		cfg.Devices = append(cfg.Devices, config.DeviceConfiguration{DeviceID: peerID, AgentcfgSync: true})
	})
	codex = filepath.Join(env.home, ".codex")
	w, err := env.cfg.Modify(func(cfg *config.Configuration) {
		cfg.SetFolder(config.FolderConfiguration{ID: "codex-config", Path: codex, FilesystemType: config.FilesystemTypeBasic})
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Wait()
	env.start(t)
	waitFor(t, "the other three folders", func() bool { return len(env.keptFolders()) == 3 })
	if _, ok := env.keptFolders()["agentcfg-codex"]; ok {
		t.Error("~/.codex kept as a second folder")
	}
	waitFor(t, "the reason", func() bool { return strings.Contains(env.svc.Status().SyncError, `"codex-config"`) })
}

func TestJoiningDeviceYieldsToTheOthers(t *testing.T) {
	env := newTestEnv(t, func(cfg *config.Configuration) {
		cfg.Devices = append(cfg.Devices, config.DeviceConfiguration{DeviceID: peerID, AgentcfgSync: true})
	})
	settings := filepath.Join(env.home, ".claude", "settings.json")
	credentials := filepath.Join(env.home, ".claude", ".credentials.json")
	ownCodex := filepath.Join(env.home, ".codex", "config.toml")
	for _, path := range []string{settings, credentials, ownCodex} {
		writeFile(t, path, "{}")
	}
	// The peer already shares Claude Code's folder, but not Codex's.
	env.scans.offer(peerID, "agentcfg-claude")
	env.start(t)
	waitFor(t, "the folders", func() bool { return len(env.keptFolders()) == 4 })

	mtime := func(path string) time.Time {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.ModTime()
	}
	if !mtime(settings).Equal(time.Unix(0, 0)) {
		t.Errorf("settings.json modified at %v; want the epoch, so the peer's copy wins", mtime(settings))
	}
	if mtime(credentials).Equal(time.Unix(0, 0)) || mtime(ownCodex).Equal(time.Unix(0, 0)) {
		t.Error("a file that is not synced, or of a folder this device starts, lost its time")
	}
}
