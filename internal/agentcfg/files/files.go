// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Package files writes configuration files atomically, through symlinks, after
// keeping a copy of what they held.
package files

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ErrChanged means the file was modified between planning and writing.
var ErrChanged = errors.New("changed while apply was running")

// Write replaces path with data if it still holds before (nil: must not
// exist). The previous content is copied into backupDir first. A symlink is
// kept: the file it resolves to is written. It returns the backup path, empty
// when the file did not exist. A non-zero mode replaces the file's own; zero
// keeps it, or gives a new file 0600.
func Write(path string, before, data []byte, mode fs.FileMode, backupDir string) (string, error) {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	current, err := os.ReadFile(target)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if exists != (before != nil) || !bytes.Equal(current, before) {
		return "", fmt.Errorf("%s: %w", path, ErrChanged)
	}

	keep := mode == 0
	if keep {
		mode = 0o600
	}
	backup := ""
	if exists {
		info, err := os.Stat(target)
		if err != nil {
			return "", err
		}
		if keep {
			mode = info.Mode().Perm()
		}
		if err := os.MkdirAll(backupDir, 0o700); err != nil {
			return "", err
		}
		// One flat name per file: the path with its separators, and a Windows
		// volume's colon, replaced.
		name := strings.NewReplacer("/", "%", ":", "%").Replace(strings.TrimPrefix(filepath.ToSlash(target), "/"))
		backup = filepath.Join(backupDir, name)
		if err := os.WriteFile(backup, current, 0o600); err != nil { //nolint:gosec // name has no separators, so it stays in backupDir
			return "", err
		}
	} else if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", err
	}

	return backup, Atomic(target, data, mode)
}

// Atomic writes data to a temporary file next to path and renames it over
// path, so readers see either the old or the new content.
func Atomic(path string, data []byte, mode fs.FileMode) error {
	return publish(path, data, mode, os.Rename)
}

// Create is Atomic for a file that must not exist yet: it fails with
// fs.ErrExist, rather than replace it, if another writer created path first.
func Create(path string, data []byte, mode fs.FileMode) error {
	return publish(path, data, mode, os.Link)
}

func publish(path string, data []byte, mode fs.FileMode, place func(tmp, path string) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".agentcfg-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return place(tmp.Name(), path)
}

// Prune keeps the newest keep entries of dir, whose names sort by age.
func Prune(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	for len(names) > keep {
		if err := os.RemoveAll(filepath.Join(dir, names[0])); err != nil {
			return err
		}
		names = names[1:]
	}
	return nil
}
