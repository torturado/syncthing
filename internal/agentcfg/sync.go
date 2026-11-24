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
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/syncthing/syncthing/internal/agentcfg/claude"
	"github.com/syncthing/syncthing/internal/agentcfg/codex"
	"github.com/syncthing/syncthing/internal/agentcfg/model"
	"github.com/syncthing/syncthing/internal/agentcfg/spec"
)

// SyncFolder is a directory of agent configuration that the machines of a
// fleet keep identical, for example with Syncthing.
type SyncFolder struct {
	// Name is the folder's key in the spec's [sync.exclude].
	Name  string `json:"name"`
	Label string `json:"label"`
	Path  string `json:"path"`
	// Ignore holds the folder's Syncthing ignore patterns, of which the
	// first match wins: the spec's exclusions, then what the agents keep on
	// each machine.
	Ignore []string `json:"ignore"`
}

// agentsJunk is what no machine needs from the directory the agents share:
// repositories, dependencies and caches that skills bring along.
var agentsJunk = []string{"**/.git", "**/node_modules", "**/__pycache__", "**/*.pyc", "**/.DS_Store"}

// geminiShared are the entries of ~/.gemini that Antigravity (Desktop, IDE
// and the agy CLI) loads as global configuration: the global rules and what
// it reads from config/. The rest belongs to one machine: config/config.json
// with its per-machine settings, config/projects, and the antigravity*
// directories with conversations, caches and installation IDs.
var geminiShared = []string{
	"GEMINI.md",
	"config/agents", "config/global_workflows", "config/hooks.json",
	"config/mcp_config.json", "config/plugins", "config/skills",
}

// SyncFolders returns the folders of agent configuration to sync: the
// directory the agents share, ~/.agents, where the skills and usually the
// spec live, and of each agent's own directory only its configuration, never
// its credentials, sessions or caches. The spec at opts.SpecPath adds its
// exclusions; while it does not exist, as on a machine that has not
// received it yet, nothing more is excluded.
func SyncFolders(opts Options) ([]SyncFolder, error) {
	home, err := homeDir(opts.Home)
	if err != nil {
		return nil, err
	}
	var exclude map[string][]string
	if opts.SpecPath != "" {
		file, err := spec.Load(opts.SpecPath)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, fmt.Errorf("spec: %w", err)
		default:
			exclude = file.Sync.Exclude
		}
	}
	folders := []SyncFolder{
		{Name: "agents", Label: "Agents", Path: filepath.Join(home, ".agents"), Ignore: agentsJunk},
		{Name: string(model.Claude), Label: "Claude Code", Path: claude.PathsFor(home).ConfigDir, Ignore: only(claude.Shared)},
		{Name: string(model.Codex), Label: "Codex", Path: codex.ConfigDir(home), Ignore: only(codex.Shared)},
		{Name: model.GeminiFolder, Label: "Antigravity", Path: filepath.Join(home, ".gemini"), Ignore: only(geminiShared)},
	}
	for i, f := range folders {
		folders[i].Ignore = append(append([]string{}, exclude[f.Name]...), f.Ignore...)
	}
	return folders, nil
}

// only returns the patterns that sync the given entries of a directory and
// nothing else in it. An entry may lie in a subdirectory, such as
// config/skills; the rest of that subdirectory stays out too.
func only(entries []string) []string {
	var patterns, parents []string
	for _, entry := range entries {
		patterns = append(patterns, "!/"+entry)
		for dir := path.Dir(entry); dir != "."; dir = path.Dir(dir) {
			if !slices.Contains(parents, dir) {
				parents = append(parents, dir)
			}
		}
	}
	// The first match wins, so a deeper directory comes before its parent:
	// its other entries are left out, then the directory itself is kept.
	slices.SortStableFunc(parents, func(a, b string) int {
		return strings.Count(b, "/") - strings.Count(a, "/")
	})
	for _, dir := range parents {
		patterns = append(patterns, "/"+dir+"/*", "!/"+dir)
	}
	return append(patterns, "/*")
}
