// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package jsonedit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"

	"github.com/syncthing/syncthing/internal/agentcfg/model"
)

// EditKey rewrites one top-level key of a JSON file. change receives the
// current value (nil when absent) and returns the new one with a description
// of each change; no changes means no edit. A missing or empty file is an
// empty object.
func EditKey(path, key string, change func(json.RawMessage) (json.RawMessage, []string, error)) (*model.FileEdit, error) {
	before, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	doc := Object{}
	if len(bytes.TrimSpace(before)) > 0 {
		if doc, err = Parse(before); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	current, _ := doc.Get(key)
	value, changes, err := change(current)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(changes) == 0 {
		return nil, nil //nolint:nilnil // no edit: the file already matches
	}
	trailingNewline := len(before) == 0 || before[len(before)-1] == '\n'
	after, err := Format(doc.Set(key, value), trailingNewline)
	if err != nil {
		return nil, err
	}
	return &model.FileEdit{Path: path, Before: before, After: after, Changes: changes}, nil
}

// Servers describes how to reconcile an mcpServers object with the spec.
type Servers struct {
	// Want holds the entries the spec declares, by name.
	Want map[string]Object
	// OwnedKeys are the keys of an entry that Want describes; the other keys
	// of an existing entry, such as a timeout, are kept.
	OwnedKeys []string
	// Owned reports whether an existing entry is agentcfg's to change.
	Owned func(name string) bool
	// Add appends the declared entries that are missing.
	Add bool
}

// Reconcile keeps the order of servers that stay, corrects the owned keys of
// those that differ, removes owned servers the spec does not declare and,
// when Add is set, appends new ones in name order.
func (s Servers) Reconcile(current json.RawMessage) (json.RawMessage, []string, error) {
	existing := Object{}
	if current != nil {
		var err error
		if existing, err = Parse(current); err != nil {
			return nil, nil, fmt.Errorf("mcpServers: %w", err)
		}
	}
	var changes []string
	next := Object{}
	for _, member := range existing {
		entry, declared := s.Want[member.Key]
		switch {
		case !s.Owned(member.Key):
			next = append(next, member)
		case !declared:
			changes = append(changes, "remove MCP server "+member.Key)
		default:
			value, err := s.merge(member.Value, entry)
			if err != nil {
				return nil, nil, fmt.Errorf("mcpServers.%s: %w", member.Key, err)
			}
			if !Equal(member.Value, value) {
				changes = append(changes, "update MCP server "+member.Key)
			}
			next = append(next, Member{Key: member.Key, Value: value})
		}
	}
	if s.Add {
		for _, name := range slices.Sorted(maps.Keys(s.Want)) {
			if _, found := existing.Get(name); !found {
				value, err := Marshal(s.Want[name])
				if err != nil {
					return nil, nil, err
				}
				changes = append(changes, "add MCP server "+name)
				next = append(next, Member{Key: name, Value: value})
			}
		}
	}
	if len(changes) == 0 {
		return current, nil, nil
	}
	value, err := Marshal(next)
	return value, changes, err
}

// merge writes the owned keys of want into an existing entry, in place, and
// keeps the other keys.
func (s Servers) merge(current json.RawMessage, want Object) (json.RawMessage, error) {
	entry, err := Parse(current)
	if err != nil {
		return nil, err
	}
	merged := Object{}
	for _, member := range entry {
		if !slices.Contains(s.OwnedKeys, member.Key) {
			merged = append(merged, member)
		} else if value, ok := want.Get(member.Key); ok {
			merged = append(merged, Member{Key: member.Key, Value: value})
		}
	}
	for _, member := range want {
		if _, found := merged.Get(member.Key); !found {
			merged = append(merged, member)
		}
	}
	return Marshal(merged)
}
