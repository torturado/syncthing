// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package agentcfg

import (
	"cmp"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
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

// reportsFolder is the folder that holds the spec, as far as the fleet
// view is concerned.
type reportsFolder struct {
	id   string
	root string // on disk
	// noShare says why this device cannot share its report there; it can
	// still read the others'.
	noShare error
}

// reportsFolderFor returns a folder's reports location, or why there is
// none.
func reportsFolderFor(cfg config.Configuration, folderID string) (reportsFolder, error) {
	if folderID == "" {
		return reportsFolder{}, errors.New("the spec is not in a shared folder, so there is nowhere to share the report")
	}
	folder, _, ok := cfg.Folder(folderID)
	if !ok {
		return reportsFolder{}, fmt.Errorf("folder %s is not configured", folderID)
	}
	if folder.Type == config.FolderTypeReceiveEncrypted {
		return reportsFolder{}, fmt.Errorf("folder %s is receive-encrypted here, so its reports cannot be read", folderID)
	}
	root, err := stfs.ExpandTilde(folder.Path)
	if err != nil {
		return reportsFolder{}, err
	}
	f := reportsFolder{id: folderID, root: root}
	if folder.Type == config.FolderTypeReceiveOnly {
		f.noShare = fmt.Errorf("folder %s is receive-only here, so this device's report would not reach the others", folderID)
	}
	return f, nil
}

// sharedFile is where this device's report is shared.
type sharedFile struct {
	folder string // folder ID
	root   string // the folder's root on disk
	name   string // within the folder, slash-separated
}

func (f sharedFile) path() string {
	return filepath.Join(f.root, filepath.FromSlash(f.name))
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
	folder, err := reportsFolderFor(cfg, folderID)
	if err == nil {
		err = folder.noShare
	}
	if err == nil {
		current = sharedFile{folder: folder.id, root: folder.root, name: path.Join(ReportsDir, s.cfg.MyID().String()+".json")}
	}
	enabled := cfg.Options.AgentcfgEnabled && cfg.Options.AgentcfgFleetEnabled

	s.shareMut.Lock()
	defer s.shareMut.Unlock()
	for _, old := range []sharedFile{s.shared, current} {
		if old.root == "" || enabled && old == current {
			continue
		}
		if err := removeReport(old); err == nil {
			slog.Info("Removed the shared agentcfg report", slogutil.FilePath(old.path()))
			s.scanShared(old)
		} else if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("Failed to remove the shared agentcfg report", slogutil.FilePath(old.path()), slogutil.Error(err))
		}
	}
	if !enabled || current != s.shared {
		// What was shared is gone, so the next report is written whatever
		// it says.
		s.sharedSig = ""
	}
	s.shared, s.shareErr = sharedFile{}, ""
	if !enabled {
		return
	}
	if current.root == "" {
		s.shareErr = err.Error()
		return
	}
	s.shared = current
	if sig := signature(st); sig != s.sharedSig {
		if err := writeReport(current, st); err != nil {
			slog.Warn("Failed to share the agentcfg report", slogutil.FilePath(current.path()), slogutil.Error(err))
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
	s.scan(f.folder, []string{f.name})
}

// scan asks for a scan of subs in a folder, or of all of it when subs is
// empty, without waiting for it.
func (s *Service) scan(folder string, subs []string) {
	if s.model == nil {
		return
	}
	go func() {
		if err := s.model.ScanFolderSubdirs(folder, subs); err != nil {
			slog.Debug("Failed to scan after an agentcfg change", slog.String("folder", folder), slog.Any("subs", subs), slogutil.Error(err))
		}
	}()
}

// The report files are reached through os.Root, so that a symlink that
// another device synced into the folder cannot lead them outside it.

// writeReport replaces the report file through a new temporary name that
// Syncthing does not scan.
func writeReport(f sharedFile, st Status) error {
	bs, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(f.root)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := checkReportsDir(root); err != nil {
		return err
	}
	if err := root.MkdirAll(path.Dir(f.name), 0o755); err != nil {
		return err
	}
	tmp := path.Join(path.Dir(f.name), stfs.UnixTempPrefix+"agentcfg-"+rand.Text()+".tmp")
	file, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = file.Write(append(bs, '\n'))
	if err = errors.Join(err, file.Close()); err == nil {
		err = root.Rename(tmp, f.name)
	}
	if err != nil {
		_ = root.Remove(tmp)
	}
	return err
}

// checkReportsDir refuses a reports directory reached through a symlink.
// os.Root keeps it inside the folder, but Syncthing syncs a symlink rather
// than what it points to, so a report written there would not reach the
// others.
func checkReportsDir(root *os.Root) error {
	dir := ""
	for _, part := range strings.Split(ReportsDir, "/") {
		dir = path.Join(dir, part)
		info, err := root.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
	}
	return nil
}

func removeReport(f sharedFile) error {
	root, err := os.OpenRoot(f.root)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := checkReportsDir(root); err != nil {
		return err
	}
	return root.Remove(f.name)
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

	id protocol.DeviceID
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

	folder, err := reportsFolderFor(cfg, st.Folder)
	if err == nil && folder.noShare != nil {
		// Known from the configuration, which share may not have seen yet.
		self.Error = folder.noShare.Error()
	}
	var peers []FleetMember
	if err == nil {
		peers, err = readReports(folder.root, myID)
	}
	if err != nil {
		switch {
		case self.Error == "":
			self.Error = err.Error()
		case self.Error != err.Error():
			self.Error += "; " + err.Error()
		}
		return []FleetMember{self}, nil
	}
	for i := range peers {
		peers[i].Name = deviceName(cfg, peers[i].id)
	}
	slices.SortFunc(peers, func(a, b FleetMember) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.DeviceID, b.DeviceID))
	})
	return append([]FleetMember{self}, peers...), nil
}

// readReports reads the reports in a folder of every device but ours.
func readReports(folderRoot string, myID protocol.DeviceID) ([]FleetMember, error) {
	root, err := os.OpenRoot(folderRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := checkReportsDir(root); err != nil {
		return nil, err
	}
	dir, err := root.Open(ReportsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return nil, err
	}
	var peers []FleetMember
	for _, entry := range entries {
		stem, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		// Conflict copies and any other name than a device ID as we write
		// it are not reports.
		id, err := protocol.DeviceIDFromString(stem)
		if err != nil || stem == "" || id.String() != stem || id == myID {
			continue
		}
		m := FleetMember{DeviceID: stem, id: id}
		if st, err := readReport(root, path.Join(ReportsDir, entry.Name())); err != nil {
			m.Error = err.Error()
		} else {
			m.Status = &st
		}
		peers = append(peers, m)
	}
	return peers, nil
}

func readReport(root *os.Root, name string) (Status, error) {
	f, err := root.Open(name)
	if err != nil {
		return Status{}, err
	}
	defer f.Close()
	bs, err := io.ReadAll(io.LimitReader(f, fleetMaxReport+1))
	if err != nil {
		return Status{}, err
	}
	if len(bs) > fleetMaxReport {
		return Status{}, fmt.Errorf("report larger than %d bytes", fleetMaxReport)
	}
	var st Status
	if err := json.Unmarshal(bs, &st); err != nil {
		return Status{}, fmt.Errorf("invalid report: %w", err)
	}
	return st, nil
}

// deviceName is the name this device knows a device by, empty if unknown.
func deviceName(cfg config.Configuration, id protocol.DeviceID) string {
	dev, _, _ := cfg.Device(id)
	return dev.Name
}
