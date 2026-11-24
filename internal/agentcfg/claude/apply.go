// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package claude

import (
	"encoding/json"
	"errors"
	"slices"

	"github.com/syncthing/syncthing/internal/agentcfg/jsonedit"
	"github.com/syncthing/syncthing/internal/agentcfg/model"
	"github.com/syncthing/syncthing/internal/agentcfg/spec"
)

// Plan computes the edits that bring Claude Code to the spec. It owns
// mcpServers in ~/.claude.json and ~/.mcp.json and the hooks in the user
// settings file; project entries, permissions and every other key are carried
// over unchanged.
//
// A declared server enabled in ~/.mcp.json is corrected there, and one enabled
// in managed settings with the declared target is left there; either way it is
// not added to ~/.claude.json, though an entry already there is corrected. Within a server entry only the keys the
// spec describes are written, so options such as a timeout are kept.
func Plan(home string, desired spec.Spec, inv model.Inventory) ([]model.FileEdit, error) {
	paths := PathsFor(home)
	var edits []model.FileEdit

	inMCPJSON := func(name string) bool { return enabledIn(inv, name, "project ~") }
	userScope, mcpJSON := map[string]jsonedit.Object{}, map[string]jsonedit.Object{}
	for name, server := range desired.MCPServers {
		if !server.AppliesTo(model.Claude) {
			continue
		}
		entry, err := render(server)
		if err != nil {
			return nil, err
		}
		elsewhere := inMCPJSON(name) || providedByManaged(name, server, inv)
		if inMCPJSON(name) {
			mcpJSON[name] = entry
		}
		// The user scope also applies outside the home directory, where
		// ~/.mcp.json does not load, so an existing user entry is corrected
		// rather than removed.
		if !elsewhere || enabledIn(inv, name, "user") {
			userScope[name] = entry
		}
	}
	for _, target := range []struct {
		path string
		want map[string]jsonedit.Object
		// owned reports whether an entry of the file is agentcfg's to change.
		owned func(string) bool
		add   bool
	}{
		{paths.State, userScope, func(string) bool { return true }, true},
		// A server in ~/.mcp.json that is not approved does not load, as
		// check does not report it; it is left alone.
		{paths.MCPJSON, mcpJSON, inMCPJSON, false},
	} {
		servers := jsonedit.Servers{Want: target.want, OwnedKeys: ownedKeys, Owned: target.owned, Add: target.add}
		edit, err := jsonedit.EditKey(target.path, "mcpServers", servers.Reconcile)
		if err != nil {
			return nil, err
		}
		if edit != nil {
			edits = append(edits, *edit)
		}
	}

	edit, err := jsonedit.EditKey(paths.Settings, "hooks", func(current json.RawMessage) (json.RawMessage, []string, error) {
		if current == nil {
			return nil, nil, nil
		}
		value, removed, err := jsonedit.FilterHooks(current, func(command string) bool {
			return slices.Contains(desired.Hooks[model.Claude], command)
		})
		var changes []string
		for _, hook := range removed {
			changes = append(changes, "remove hook "+hook)
		}
		return value, changes, err
	})
	if err != nil {
		return nil, err
	}
	if edit != nil {
		edits = append(edits, *edit)
	}
	return edits, nil
}

func enabledIn(inv model.Inventory, name, scope string) bool {
	return slices.ContainsFunc(inv.MCPServers, func(s model.MCPServer) bool {
		return s.Agent == model.Claude && s.Name == name && s.Enabled && s.Scope == scope
	})
}

func providedByManaged(name string, want spec.Server, inv model.Inventory) bool {
	return slices.ContainsFunc(inv.MCPServers, func(s model.MCPServer) bool {
		return s.Agent == model.Claude && s.Name == name && s.Enabled && s.Scope == "managed" &&
			s.URL == want.URL && s.Command == want.Command
	})
}

// ownedKeys are the keys of a server entry that render writes.
var ownedKeys = []string{"type", "url", "headers", "command", "args", "env"}

// render is a spec server as a Claude Code mcpServers entry, with its keys in
// the order Claude Code writes them.
func render(s spec.Server) (jsonedit.Object, error) {
	entry := jsonedit.Object{}
	add := func(key string, value any) error {
		raw, err := jsonedit.Marshal(value)
		entry = entry.Set(key, raw)
		return err
	}
	var err error
	if s.URL != "" {
		err = errors.Join(add("type", "http"), add("url", s.URL))
		if len(s.Headers) > 0 {
			err = errors.Join(err, add("headers", s.Headers))
		}
		return entry, err
	}
	err = errors.Join(add("type", "stdio"), add("command", s.Command))
	if len(s.Args) > 0 {
		err = errors.Join(err, add("args", s.Args))
	}
	if len(s.Env) > 0 {
		err = errors.Join(err, add("env", s.Env))
	}
	return entry, err
}
