// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package jsonedit

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/syncthing/syncthing/internal/agentcfg/model"
)

// FilterHooks removes from a hooks object (event → matcher groups → handlers)
// every handler whose command is not allowed, then any group and event left
// empty. It returns the hooks it removed as "event: command".
func FilterHooks(raw json.RawMessage, allowed func(command string) bool) (json.RawMessage, []string, error) {
	events, err := Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("hooks: %w", err)
	}
	kept, removed, err := filterEvents(events, allowed, false, "hooks.")
	if err != nil || len(removed) == 0 {
		return raw, nil, err
	}
	value, err := Marshal(kept)
	return value, removed, err
}

// FilterNamedHooks does what FilterHooks does to an Antigravity hooks.json:
// named hooks, each with an optional "enabled" and its events, where an
// event's entry is either a matcher group or a handler itself. A hook with
// "enabled": false does not run and is kept as it is; a hook left without
// events is removed.
func FilterNamedHooks(data []byte, allowed func(command string) bool) ([]byte, []string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return data, nil, nil
	}
	hooks, err := Parse(data)
	if err != nil {
		return nil, nil, err
	}
	var removed []string
	kept := Object{}
	for _, hook := range hooks {
		entry, err := Parse(hook.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", hook.Key, err)
		}
		if enabled, ok := entry.Get("enabled"); ok && string(bytes.TrimSpace(enabled)) == "false" {
			kept = append(kept, hook)
			continue
		}
		var events, other Object
		for _, member := range entry {
			if member.Key == "enabled" {
				other = append(other, member)
			} else {
				events = append(events, member)
			}
		}
		keptEvents, hookRemoved, err := filterEvents(events, allowed, true, hook.Key+".")
		if err != nil {
			return nil, nil, err
		}
		if len(hookRemoved) == 0 {
			kept = append(kept, hook)
			continue
		}
		removed = append(removed, hookRemoved...)
		if len(keptEvents) == 0 {
			continue
		}
		value, err := Marshal(append(other, keptEvents...))
		if err != nil {
			return nil, nil, err
		}
		kept = append(kept, Member{hook.Key, value})
	}
	if len(removed) == 0 {
		return data, nil, nil
	}
	after, err := Format(kept, data[len(data)-1] == '\n')
	return after, removed, err
}

// filterEvents filters each event's groups. With direct, a group that has a
// command and no "hooks" is a handler itself.
func filterEvents(events Object, allowed func(command string) bool, direct bool, where string) (Object, []string, error) {
	var removed []string
	kept := Object{}
	for _, event := range events {
		var groups []json.RawMessage
		if err := json.Unmarshal(event.Value, &groups); err != nil {
			return nil, nil, fmt.Errorf("%s%s: %w", where, event.Key, err)
		}
		var keptGroups []json.RawMessage
		changed := false
		for _, rawGroup := range groups {
			group, err := Parse(rawGroup)
			if err != nil {
				return nil, nil, fmt.Errorf("%s%s: %w", where, event.Key, err)
			}
			rawHandlers, nested := group.Get("hooks")
			if _, isHandler := group.Get("command"); direct && !nested && isHandler {
				var handler model.HookHandler
				if err := json.Unmarshal(rawGroup, &handler); err != nil {
					return nil, nil, fmt.Errorf("%s%s: %w", where, event.Key, err)
				}
				if allowed(handler.HookCommand()) {
					keptGroups = append(keptGroups, rawGroup)
				} else {
					changed = true
					removed = append(removed, event.Key+": "+handler.HookCommand())
				}
				continue
			}
			var handlers []json.RawMessage
			if rawHandlers != nil {
				if err := json.Unmarshal(rawHandlers, &handlers); err != nil {
					return nil, nil, fmt.Errorf("%s%s: %w", where, event.Key, err)
				}
			}
			var keptHandlers []json.RawMessage
			for _, rawHandler := range handlers {
				var handler model.HookHandler
				if err := json.Unmarshal(rawHandler, &handler); err != nil {
					return nil, nil, fmt.Errorf("%s%s: %w", where, event.Key, err)
				}
				if allowed(handler.HookCommand()) {
					keptHandlers = append(keptHandlers, rawHandler)
				} else {
					removed = append(removed, event.Key+": "+handler.HookCommand())
				}
			}
			if len(keptHandlers) == len(handlers) {
				keptGroups = append(keptGroups, rawGroup)
				continue
			}
			changed = true
			if len(keptHandlers) == 0 {
				continue
			}
			value, err := Marshal(keptHandlers)
			if err != nil {
				return nil, nil, err
			}
			newGroup, err := Marshal(group.Set("hooks", value))
			if err != nil {
				return nil, nil, err
			}
			keptGroups = append(keptGroups, newGroup)
		}
		switch {
		case !changed:
			kept = append(kept, event)
		case len(keptGroups) > 0:
			value, err := Marshal(keptGroups)
			if err != nil {
				return nil, nil, err
			}
			kept = append(kept, Member{event.Key, value})
		}
	}
	return kept, removed, nil
}
