// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package agentcfg

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/syncthing/syncthing/internal/slogutil"
	"github.com/syncthing/syncthing/lib/config"
	stfs "github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/protocol"
)

const (
	// ReportsDir is where, within the folder that holds the spec, each
	// device shares its status as <device ID>.json.
	ReportsDir = ".agentcfg/reports"

	fleetMaxReport = 1 << 20
)

var errFleetDisabled = errors.New("the agentcfg fleet view is disabled")

func (s *Service) fleetEnabled() bool {
	opts := s.cfg.Options()
	return opts.AgentcfgEnabled && opts.AgentcfgFleetEnabled
}

// reportsDir returns the reports directory of a shared folder, or why this
// device cannot share its report there.
func reportsDir(cfg config.Configuration, folderID string) (string, error) {
	if folderID == "" {
		return "", errors.New("the spec is not in a shared folder, so there is nowhere to share the report")
	}
	folder, _, ok := cfg.Folder(folderID)
	if !ok {
		return "", fmt.Errorf("folder %s is not configured", folderID)
	}
	switch folder.Type {
	case config.FolderTypeReceiveOnly, config.FolderTypeReceiveEncrypted:
		return "", fmt.Errorf("folder %s is %s here, so this device's report would not reach the others", folderID, folder.Type)
	}
	root, err := stfs.ExpandTilde(folder.Path)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(ReportsDir)), nil
}

// sharedFile is where this device's report is shared.
type sharedFile struct {
	folder string // folder ID
	path   string // on disk
}

// share keeps this device's status in the folder that holds the spec while
// the fleet view is on, and removes it otherwise. The file is rewritten only
// when what the status says changes, so the folder sees one change per
// change of result.
func (s *Service) share(st Status) {
	cfg := s.cfg.RawCopy()
	folderID := st.Folder
	if folderID == "" {
		// Disabled, or the spec could not be read: the folder is still
		// where an earlier report may have to be removed from.
		if target, err := findSpec(cfg); err == nil {
			folderID = target.folder
		}
	}
	var current sharedFile
	dir, err := reportsDir(cfg, folderID)
	if err == nil {
		current = sharedFile{folder: folderID, path: filepath.Join(dir, s.cfg.MyID().String()+".json")}
	}
	enabled := cfg.Options.AgentcfgEnabled && cfg.Options.AgentcfgFleetEnabled

	s.shareMut.Lock()
	defer s.shareMut.Unlock()
	for _, old := range []sharedFile{s.shared, current} {
		if old.path == "" || enabled && old == current {
			continue
		}
		if err := os.Remove(old.path); err == nil {
			slog.Info("Removed the shared agentcfg report", slogutil.FilePath(old.path))
			s.scanShared(old)
		} else if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("Failed to remove the shared agentcfg report", slogutil.FilePath(old.path), slogutil.Error(err))
		}
	}
	s.shared, s.shareErr = sharedFile{}, ""
	if !enabled {
		s.sharedSig = ""
		return
	}
	if current.path == "" {
		s.shareErr = err.Error()
		return
	}
	s.shared = current
	if sig := current.path + "\x02" + signature(st); sig != s.sharedSig {
		if err := writeReport(current.path, st); err != nil {
			slog.Warn("Failed to share the agentcfg report", slogutil.FilePath(current.path), slogutil.Error(err))
			s.shareErr = err.Error()
			s.sharedSig = ""
			return
		}
		s.sharedSig = sig
		s.scanShared(current)
	}
}

// scanShared tells Syncthing about a change to the shared report rather
// than waiting for the folder's watcher, which may be off, or may start
// only after the change, such as one made just after startup. A folder that
// is not running yet scans the file when it starts.
func (s *Service) scanShared(f sharedFile) {
	if s.scanner == nil {
		return
	}
	sub := ReportsDir + "/" + filepath.Base(f.path)
	go func() {
		if err := s.scanner.ScanFolderSubdirs(f.folder, []string{sub}); err != nil {
			slog.Debug("Failed to scan the shared agentcfg report", slogutil.FilePath(f.path), slogutil.Error(err))
		}
	}()
}

// writeReport replaces the report file through a temporary name that
// Syncthing does not scan.
func writeReport(path string, st Status) error {
	bs, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := stfs.TempName(path)
	if err := os.WriteFile(tmp, append(bs, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// FleetMember is one device in the fleet view.
type FleetMember struct {
	DeviceID string  `json:"deviceID"`
	Name     string  `json:"name"`
	Self     bool    `json:"self"`
	Status   *Status `json:"status"`
	// Error says why this device's report is not shared, or why a peer's
	// report could not be read.
	Error string `json:"error"`
}

// Fleet returns the status of this device, followed by the reports that the
// other devices shared in the folder that holds the spec.
func (s *Service) Fleet() ([]FleetMember, error) {
	if !s.fleetEnabled() {
		return nil, errFleetDisabled
	}
	cfg := s.cfg.RawCopy()
	st := s.Status()
	myID := s.cfg.MyID()
	s.shareMut.Lock()
	self := FleetMember{DeviceID: myID.String(), Name: deviceName(cfg, myID), Self: true, Status: &st, Error: s.shareErr}
	s.shareMut.Unlock()

	dir, err := reportsDir(cfg, st.Folder)
	if err != nil {
		if self.Error == "" {
			self.Error = err.Error()
		}
		return []FleetMember{self}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var peers []FleetMember
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		// Conflict copies and anything else not named by a device ID are
		// not reports.
		id, err := protocol.DeviceIDFromString(name)
		if err != nil || id == myID {
			continue
		}
		m := FleetMember{DeviceID: id.String(), Name: deviceName(cfg, id)}
		if peer, err := readReport(filepath.Join(dir, entry.Name())); err != nil {
			m.Error = err.Error()
		} else {
			m.Status = &peer
		}
		peers = append(peers, m)
	}
	slices.SortFunc(peers, func(a, b FleetMember) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.DeviceID, b.DeviceID))
	})
	return append([]FleetMember{self}, peers...), nil
}

func readReport(path string) (Status, error) {
	f, err := os.Open(path)
	if err != nil {
		return Status{}, err
	}
	defer f.Close()
	var st Status
	if err := json.NewDecoder(io.LimitReader(f, fleetMaxReport)).Decode(&st); err != nil {
		return Status{}, fmt.Errorf("invalid report: %w", err)
	}
	return st, nil
}

// deviceName is the name this device knows a device by, empty if unknown.
func deviceName(cfg config.Configuration, id protocol.DeviceID) string {
	dev, _, _ := cfg.Device(id)
	return dev.Name
}
