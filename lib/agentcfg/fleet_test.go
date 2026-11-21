// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package agentcfg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
)

var (
	peerID   = protocol.DeviceID{1}
	brokenID = protocol.DeviceID{2}
)

// scanRecorder stands in for the model and records the scans asked for.
type scanRecorder struct {
	mut   sync.Mutex
	scans []string
}

func (r *scanRecorder) ScanFolderSubdirs(folder string, subs []string) error {
	r.mut.Lock()
	defer r.mut.Unlock()
	for _, sub := range subs {
		r.scans = append(r.scans, folder+":"+sub)
	}
	return nil
}

// count returns how many scans of sub in folder were asked for.
func (r *scanRecorder) count(folder, sub string) int {
	r.mut.Lock()
	defer r.mut.Unlock()
	n := 0
	for _, s := range r.scans {
		if s == folder+":"+sub {
			n++
		}
	}
	return n
}

func (env *testEnv) reportPath(id protocol.DeviceID) string {
	return filepath.Join(env.folder, filepath.FromSlash(ReportsDir), id.String()+".json")
}

func sharedStatus(t *testing.T, path string) (Status, bool) {
	t.Helper()
	bs, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Status{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	if err := json.Unmarshal(bs, &st); err != nil {
		t.Fatal(err)
	}
	return st, true
}

func TestFleetSharesReportsThroughTheSpecFolder(t *testing.T) {
	env := newTestEnv(t, func(cfg *config.Configuration) {
		cfg.Options.AgentcfgFleetEnabled = true
		cfg.Devices = append(cfg.Devices, config.DeviceConfiguration{DeviceID: peerID, Name: "peer"})
	})
	writeFile(t, filepath.Join(env.folder, SpecFileName), testSpec)
	env.start(t)

	// This device shares its report in the folder that holds the spec.
	own := env.reportPath(protocol.LocalDeviceID)
	waitFor(t, "the shared report", func() bool {
		st, ok := sharedStatus(t, own)
		return ok && reportErrors(st) == 1
	})
	first, _ := sharedStatus(t, own)
	// Syncthing is told about the write rather than left to its watcher.
	ownSub := ReportsDir + "/" + protocol.LocalDeviceID.String() + ".json"
	waitFor(t, "a scan of the shared report", func() bool { return env.scans.count("agents", ownSub) == 1 })

	// What the other devices shared arrives in the same directory.
	peer := Status{Spec: "/home/peer/agentcfg.toml", Folder: "agents", Error: "peer error"}
	bs, _ := json.Marshal(peer)
	writeFile(t, env.reportPath(peerID), string(bs))
	writeFile(t, env.reportPath(brokenID), "{not json")
	writeFile(t, filepath.Join(filepath.Dir(own), peerID.String()+".sync-conflict-20261121-120000-AAAAAAA.json"), string(bs))
	writeFile(t, filepath.Join(filepath.Dir(own), "notes.json"), "{}")

	fleet, err := env.svc.Fleet()
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet) != 3 {
		t.Fatalf("fleet = %+v; want this device and two peers", fleet)
	}
	if m := fleet[0]; !m.Self || m.DeviceID != protocol.LocalDeviceID.String() || reportErrors(*m.Status) != 1 || m.Error != "" {
		t.Errorf("first member = %+v; want this device and its status", m)
	}
	// Sorted by name: the unnamed device comes before "peer".
	if m := fleet[1]; m.DeviceID != brokenID.String() || m.Status != nil || !strings.Contains(m.Error, "invalid report") {
		t.Errorf("broken peer = %+v; want an error", m)
	}
	if m := fleet[2]; m.Name != "peer" || m.Status == nil || m.Status.Error != "peer error" || m.Error != "" {
		t.Errorf("peer = %+v; want its shared status", m)
	}

	// A run with the same result leaves the shared file alone, so the folder
	// does not change on every check.
	runs := env.runs.Load()
	w, err := env.cfg.Modify(func(cfg *config.Configuration) { cfg.Options.AgentcfgIntervalS = 1800 })
	if err != nil {
		t.Fatal(err)
	}
	w.Wait()
	waitFor(t, "another run", func() bool { return env.runs.Load() > runs && env.svc.Status().Report != nil })
	if again, _ := sharedStatus(t, own); !again.Report.CheckedAt.Equal(first.Report.CheckedAt) {
		t.Errorf("shared report rewritten with the same result: checked at %v, then %v", first.Report.CheckedAt, again.Report.CheckedAt)
	}

	// Turning the fleet view off withdraws this device's report, and only it.
	w, err = env.cfg.Modify(func(cfg *config.Configuration) { cfg.Options.AgentcfgFleetEnabled = false })
	if err != nil {
		t.Fatal(err)
	}
	w.Wait()
	waitFor(t, "the report to be withdrawn", func() bool {
		_, ok := sharedStatus(t, own)
		return !ok
	})
	if _, ok := sharedStatus(t, env.reportPath(peerID)); !ok {
		t.Error("a peer's report was removed")
	}
	waitFor(t, "a scan of the removal", func() bool { return env.scans.count("agents", ownSub) == 2 })
	if _, err := env.svc.Fleet(); !errors.Is(err, errFleetDisabled) {
		t.Errorf("Fleet while disabled: %v; want %v", err, errFleetDisabled)
	}
}

func TestFleetDoesNotShareFromAReceiveOnlyFolder(t *testing.T) {
	env := newTestEnv(t, func(cfg *config.Configuration) {
		cfg.Options.AgentcfgFleetEnabled = true
		cfg.Folders[0].Type = config.FolderTypeReceiveOnly
	})
	writeFile(t, filepath.Join(env.folder, SpecFileName), testSpec)
	env.start(t)
	waitFor(t, "the first check", func() bool { return env.svc.Status().Report != nil })

	fleet, err := env.svc.Fleet()
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet) != 1 || !strings.Contains(fleet[0].Error, "receiveonly") {
		t.Errorf("fleet = %+v; want only this device, saying why its report is not shared", fleet)
	}
	if _, ok := sharedStatus(t, env.reportPath(protocol.LocalDeviceID)); ok {
		t.Error("the report was written into a receive-only folder")
	}
}
