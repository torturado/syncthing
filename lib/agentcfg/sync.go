// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package agentcfg

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	agentcfglib "github.com/syncthing/syncthing/internal/agentcfg"
	"github.com/syncthing/syncthing/lib/config"
	stfs "github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/ignore"
	"github.com/syncthing/syncthing/lib/protocol"
)

// SyncFolderPrefix starts the ID of every folder of agent configuration
// that the service keeps, agentcfg-<name>, the same on every device so that
// the folders meet without anyone choosing an ID or a path.
const SyncFolderPrefix = "agentcfg-"

// ignoreHeader opens the .stignore of a kept folder, which the service
// rewrites whenever the patterns change.
var ignoreHeader = []string{
	"// Written by agentcfg in Syncthing, which replaces this file when the patterns change.",
	"// To keep more files out of every device, add them to [sync.exclude] in the agentcfg spec.",
}

// syncPeers are the devices marked agentcfgSync, sorted.
func syncPeers(cfg config.Configuration, myID protocol.DeviceID) []protocol.DeviceID {
	var peers []protocol.DeviceID
	for _, d := range cfg.Devices {
		if d.AgentcfgSync && !d.Untrusted && d.DeviceID != myID {
			peers = append(peers, d.DeviceID)
		}
	}
	slices.SortFunc(peers, func(a, b protocol.DeviceID) int { return a.Compare(b) })
	return peers
}

// syncFolders keeps a folder for each directory of agent configuration,
// shared with exactly the devices marked agentcfgSync, and removes the
// folders once no device is marked; removing a folder leaves its files.
// specPath, when not empty, is the spec whose exclusions apply.
func (s *Service) syncFolders(specPath string) error {
	cfg := s.cfg.RawCopy()
	myID := s.cfg.MyID()
	peers := syncPeers(cfg, myID)

	var wanted []agentcfglib.SyncFolder
	if len(peers) > 0 {
		var err error
		wanted, err = agentcfglib.SyncFolders(agentcfglib.Options{SpecPath: specPath, Home: s.home})
		if err != nil {
			return err
		}
	}

	var errs []error
	var changed []config.FolderConfiguration
	kept := map[string]bool{}
	for _, want := range wanted {
		id := SyncFolderPrefix + want.Name
		kept[id] = true
		lines := append(slices.Clone(ignoreHeader), want.Ignore...)
		folder, _, exists := cfg.Folder(id)
		if !exists {
			if other := folderAt(cfg, want.Path); other != "" {
				errs = append(errs, fmt.Errorf("%s is already shared as folder %q; remove that folder from Syncthing, which leaves the files, so that agentcfg can share it with every device", want.Path, other))
				continue
			}
			folder = s.cfg.DefaultFolder()
			folder.ID, folder.Label, folder.Path = id, want.Label, want.Path
			folder.FilesystemType = config.FilesystemTypeBasic
			folder.Type = config.FolderTypeSendReceive
			// Written before the folder starts, so that its first scan
			// already leaves out what stays on this device.
			if err := writeIgnores(folder, lines); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", want.Path, err))
				continue
			}
			if s.offered(id, peers) {
				// The others already share it: this device joins them.
				if err := yieldFiles(folder); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", want.Path, err))
					continue
				}
				slog.Info("Joining the agent configuration of the other devices; where a file differs, theirs is kept and this device's becomes a conflict copy", folder.LogAttr())
			}
		} else if !hasIgnores(folder, lines) {
			if err := writeIgnores(folder, lines); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", want.Path, err))
				continue
			}
			// A scan reloads the patterns.
			s.scan(id, nil)
		}
		devices := folderDevices(folder, myID, peers)
		if !exists || folder.Path != want.Path || !slices.Equal(folder.DeviceIDs(), deviceIDs(devices)) {
			folder.Path, folder.Devices = want.Path, devices
			changed = append(changed, folder)
		}
	}

	var removed []string
	for _, f := range cfg.Folders {
		if strings.HasPrefix(f.ID, SyncFolderPrefix) && !kept[f.ID] {
			removed = append(removed, f.ID)
		}
	}
	if len(changed) > 0 || len(removed) > 0 {
		w, err := s.cfg.Modify(func(cfg *config.Configuration) {
			cfg.SetFolders(changed)
			cfg.Folders = slices.DeleteFunc(cfg.Folders, func(f config.FolderConfiguration) bool { return slices.Contains(removed, f.ID) })
		})
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		w.Wait()
		for _, f := range changed {
			slog.Info("Sharing agent configuration", f.LogAttr(), slog.Int("devices", len(f.Devices)-1))
		}
		for _, id := range removed {
			slog.Info("Stopped sharing agent configuration, as no device is marked for it; the files stay", slog.String("folder", id))
		}
	}
	return errors.Join(errs...)
}

// folderAt returns the ID of the folder at path, if any.
func folderAt(cfg config.Configuration, path string) string {
	for _, f := range cfg.Folders {
		if p, err := stfs.ExpandTilde(f.Path); err == nil && filepath.Clean(p) == filepath.Clean(path) {
			return f.ID
		}
	}
	return ""
}

// folderDevices returns this device and peers, keeping what the folder
// already records about each, sorted as the configuration sorts them.
func folderDevices(folder config.FolderConfiguration, myID protocol.DeviceID, peers []protocol.DeviceID) []config.FolderDeviceConfiguration {
	devices := []config.FolderDeviceConfiguration{}
	for _, id := range append([]protocol.DeviceID{myID}, peers...) {
		dev := config.FolderDeviceConfiguration{DeviceID: id}
		if i := slices.IndexFunc(folder.Devices, func(d config.FolderDeviceConfiguration) bool { return d.DeviceID == id }); i >= 0 {
			dev = folder.Devices[i]
		}
		devices = append(devices, dev)
	}
	slices.SortFunc(devices, func(a, b config.FolderDeviceConfiguration) int { return a.DeviceID.Compare(b.DeviceID) })
	return devices
}

func deviceIDs(devices []config.FolderDeviceConfiguration) []protocol.DeviceID {
	ids := make([]protocol.DeviceID, len(devices))
	for i, d := range devices {
		ids[i] = d.DeviceID
	}
	return ids
}

// offered reports whether a marked device already offers the folder.
func (s *Service) offered(id string, peers []protocol.DeviceID) bool {
	if s.model == nil {
		return false
	}
	for _, peer := range peers {
		if pending, err := s.model.PendingFolders(peer); err == nil {
			if _, ok := pending[id]; ok {
				return true
			}
		}
	}
	return false
}

// hasIgnores reports whether the folder's .stignore holds exactly lines.
func hasIgnores(folder config.FolderConfiguration, lines []string) bool {
	matcher := ignore.New(folder.Filesystem())
	return matcher.Load(".stignore") == nil && slices.Equal(matcher.Lines(), lines)
}

func writeIgnores(folder config.FolderConfiguration, lines []string) error {
	if err := folder.CreateRoot(); err != nil {
		return err
	}
	return ignore.WriteIgnores(folder.Filesystem(), ".stignore", lines)
}

// yieldFiles dates the files that a joining device would sync back to the
// epoch. Syncthing resolves a conflict in favour of the newer file, so the
// devices that already share the folder keep their version of every file
// that differs, and the joining device keeps its own as a conflict copy.
// Identical files meet without a conflict.
func yieldFiles(folder config.FolderConfiguration) error {
	matcher := ignore.New(folder.Filesystem())
	if err := matcher.Load(".stignore"); err != nil {
		return err
	}
	root, err := stfs.ExpandTilde(folder.Path)
	if err != nil {
		return err
	}
	epoch := time.Unix(0, 0)
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		if stfs.IsInternal(rel) || matcher.Match(rel).IsIgnored() {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return os.Chtimes(path, epoch, epoch)
	})
}
