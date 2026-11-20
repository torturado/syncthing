// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package agentcfg

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/syncthing/notify"
	agentcfglib "github.com/torturado/agentcfg"

	"github.com/syncthing/syncthing/internal/slogutil"
)

// watchFiles returns the files whose changes can change the result of a run:
// the agents' files and the spec, each also under the name its symlinks
// resolve to.
func watchFiles(opts agentcfglib.Options) ([]string, error) {
	paths, err := agentcfglib.WatchPaths(opts)
	if opts.SpecPath != "" {
		paths = append(paths, opts.SpecPath)
	}
	var files []string
	for _, path := range paths {
		files = append(files, filepath.Clean(path))
		if target, err := filepath.EvalSymlinks(path); err == nil {
			files = append(files, target)
		}
	}
	slices.Sort(files)
	return slices.Compact(files), err
}

// fileStamp is what a file looked like; the zero value means it was absent.
type fileStamp struct {
	size    int64
	modTime int64
	mode    os.FileMode
}

func stampFiles(files []string) map[string]fileStamp {
	stamps := make(map[string]fileStamp, len(files))
	for _, path := range files {
		var stamp fileStamp
		if info, err := os.Stat(path); err == nil {
			stamp = fileStamp{size: info.Size(), modTime: info.ModTime().UnixNano(), mode: info.Mode()}
		}
		stamps[path] = stamp
	}
	return stamps
}

// fileWatch relays changes to a set of files. Agents and agentcfg replace
// files by renaming a new one over them, which a watch on the file itself
// would not follow, so it watches the directories that hold them.
type fileWatch struct {
	events chan notify.EventInfo
	dirs   []string
	files  []string // sorted
}

func newFileWatch() *fileWatch {
	return &fileWatch{events: make(chan notify.EventInfo, 128)}
}

// set watches the given files. Directories that do not exist yet are not
// watched; the interval covers files created in them.
func (w *fileWatch) set(files []string) {
	w.files = files
	var dirs []string
	for _, path := range files {
		dir := filepath.Dir(path)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			dirs = append(dirs, dir)
		}
	}
	slices.Sort(dirs)
	dirs = slices.Compact(dirs)
	if slices.Equal(dirs, w.dirs) {
		return
	}
	notify.Stop(w.events)
	w.dirs = nil
	for _, dir := range dirs {
		if err := notify.Watch(dir, w.events, notify.All); err != nil {
			slog.Warn("Failed to watch agent configuration directory", slogutil.FilePath(dir), slogutil.Error(err))
			continue
		}
		w.dirs = append(w.dirs, dir)
	}
}

func (w *fileWatch) matches(path string) bool {
	_, ok := slices.BinarySearch(w.files, filepath.Clean(path))
	return ok
}

func (w *fileWatch) stop() {
	notify.Stop(w.events)
}
