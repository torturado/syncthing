// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package antigravity

import (
	"errors"
	"os"
	"slices"

	"github.com/syncthing/syncthing/internal/agentcfg/jsonedit"
	"github.com/syncthing/syncthing/internal/agentcfg/model"
	"github.com/syncthing/syncthing/internal/agentcfg/spec"
)

// ownedKeys are the keys of a server entry that render writes, plus those it
// replaces: url, which serverUrl supersedes, and disabled, since a declared
// server must run.
var ownedKeys = []string{"serverUrl", "url", "headers", "command", "args", "env", "disabled"}

// Plan computes the edits that bring Antigravity to the spec: the servers
// in ~/.gemini/config/mcp_config.json and the hooks in hooks.json. Other
// keys of a server entry, such as timeoutSeconds or enabledTools, are kept.
//
// Antigravity does not expand ${VAR}, so the values are written from lookup,
// the environment apply runs in. A declared server with a variable lookup
// does not know is left as it is, and so is a disabled server the spec does
// not declare, as check does not report it.
func Plan(home string, desired spec.Spec, inv model.Inventory, lookup func(string) (string, bool)) ([]model.FileEdit, error) {
	if slices.Contains(inv.Absent, model.Antigravity) {
		return nil, nil
	}
	paths := PathsFor(home)
	want := map[string]jsonedit.Object{}
	unresolved := map[string]bool{}
	for name, server := range desired.MCPServers {
		if !server.AppliesTo(model.Antigravity) {
			continue
		}
		expanded, missing := server.Expand(lookup)
		if len(missing) > 0 {
			unresolved[name] = true
			continue
		}
		entry, err := render(expanded)
		if err != nil {
			return nil, err
		}
		want[name] = entry
	}
	enabled := func(name string) bool {
		return slices.ContainsFunc(inv.MCPServers, func(s model.MCPServer) bool {
			return s.Agent == model.Antigravity && s.Scope == "user" && s.Name == name && s.Enabled
		})
	}
	servers := jsonedit.Servers{
		Want: want, OwnedKeys: ownedKeys, Add: true,
		Owned: func(name string) bool {
			_, declared := want[name]
			return declared || !unresolved[name] && enabled(name)
		},
	}
	var edits []model.FileEdit
	edit, err := jsonedit.EditKey(paths.MCPConfig, "mcpServers", servers.Reconcile)
	if err != nil {
		return nil, err
	}
	if edit != nil {
		edits = append(edits, *edit)
	}

	before, err := os.ReadFile(paths.Hooks)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	after, removed, err := jsonedit.FilterNamedHooks(before, func(command string) bool {
		return slices.Contains(desired.Hooks[model.Antigravity], command)
	})
	if err != nil {
		return nil, err
	}
	if len(removed) > 0 {
		hooksEdit := model.FileEdit{Path: paths.Hooks, Before: before, After: after}
		for _, hook := range removed {
			hooksEdit.Changes = append(hooksEdit.Changes, "remove hook "+hook)
		}
		edits = append(edits, hooksEdit)
	}
	return edits, nil
}

// render is an expanded spec server as an mcp_config.json entry.
func render(s spec.Server) (jsonedit.Object, error) {
	entry := jsonedit.Object{}
	var errs []error
	add := func(key string, value any) {
		raw, err := jsonedit.Marshal(value)
		entry = entry.Set(key, raw)
		errs = append(errs, err)
	}
	if s.URL != "" {
		add("serverUrl", s.URL)
		if len(s.Headers) > 0 {
			add("headers", s.Headers)
		}
		return entry, errors.Join(errs...)
	}
	add("command", s.Command)
	if len(s.Args) > 0 {
		add("args", s.Args)
	}
	if len(s.Env) > 0 {
		add("env", s.Env)
	}
	return entry, errors.Join(errs...)
}
