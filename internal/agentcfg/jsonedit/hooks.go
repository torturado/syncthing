package jsonedit

import (
	"encoding/json"
	"fmt"

	"github.com/torturado/agentcfg/internal/model"
)

// FilterHooks removes from a hooks object (event → matcher groups → handlers)
// every handler whose command is not allowed, then any group and event left
// empty. It returns the hooks it removed as "event: command".
func FilterHooks(raw json.RawMessage, allowed func(command string) bool) (json.RawMessage, []string, error) {
	events, err := Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("hooks: %w", err)
	}
	var removed []string
	kept := Object{}
	for _, event := range events {
		var groups []json.RawMessage
		if err := json.Unmarshal(event.Value, &groups); err != nil {
			return nil, nil, fmt.Errorf("hooks.%s: %w", event.Key, err)
		}
		var keptGroups []json.RawMessage
		changed := false
		for _, rawGroup := range groups {
			group, err := Parse(rawGroup)
			if err != nil {
				return nil, nil, fmt.Errorf("hooks.%s: %w", event.Key, err)
			}
			rawHandlers, _ := group.Get("hooks")
			var handlers []json.RawMessage
			if rawHandlers != nil {
				if err := json.Unmarshal(rawHandlers, &handlers); err != nil {
					return nil, nil, fmt.Errorf("hooks.%s: %w", event.Key, err)
				}
			}
			var keptHandlers []json.RawMessage
			for _, rawHandler := range handlers {
				var handler model.HookHandler
				if err := json.Unmarshal(rawHandler, &handler); err != nil {
					return nil, nil, fmt.Errorf("hooks.%s: %w", event.Key, err)
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
	if len(removed) == 0 {
		return raw, nil, nil
	}
	value, err := Marshal(kept)
	return value, removed, err
}
