package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"

	"github.com/torturado/agentcfg/internal/jsonedit"
	"github.com/torturado/agentcfg/internal/model"
	"github.com/torturado/agentcfg/internal/spec"
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
		edit, err := editObject(target.path, "mcpServers", func(current json.RawMessage) (json.RawMessage, []string, error) {
			return reconcileServers(current, target.want, target.owned, target.add)
		})
		if err != nil {
			return nil, err
		}
		if edit != nil {
			edits = append(edits, *edit)
		}
	}

	edit, err := editObject(paths.Settings, "hooks", func(current json.RawMessage) (json.RawMessage, []string, error) {
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

// reconcileServers keeps the order of servers that stay, corrects the owned
// keys of those that differ and, when add is set, appends new ones in name
// order. Entries that owned rejects are kept as they are.
func reconcileServers(current json.RawMessage, want map[string]jsonedit.Object, owned func(string) bool, add bool) (json.RawMessage, []string, error) {
	existing := jsonedit.Object{}
	if current != nil {
		var err error
		if existing, err = jsonedit.Parse(current); err != nil {
			return nil, nil, fmt.Errorf("mcpServers: %w", err)
		}
	}
	var changes []string
	next := jsonedit.Object{}
	for _, member := range existing {
		entry, declared := want[member.Key]
		switch {
		case !owned(member.Key):
			next = append(next, member)
		case !declared:
			changes = append(changes, "remove MCP server "+member.Key)
		default:
			value, err := mergeEntry(member.Value, entry)
			if err != nil {
				return nil, nil, fmt.Errorf("mcpServers.%s: %w", member.Key, err)
			}
			if !jsonedit.Equal(member.Value, value) {
				changes = append(changes, "update MCP server "+member.Key)
			}
			next = append(next, jsonedit.Member{Key: member.Key, Value: value})
		}
	}
	if add {
		for _, name := range slices.Sorted(maps.Keys(want)) {
			if _, found := existing.Get(name); !found {
				value, err := jsonedit.Marshal(want[name])
				if err != nil {
					return nil, nil, err
				}
				changes = append(changes, "add MCP server "+name)
				next = append(next, jsonedit.Member{Key: name, Value: value})
			}
		}
	}
	if len(changes) == 0 {
		return current, nil, nil
	}
	value, err := jsonedit.Marshal(next)
	return value, changes, err
}

// mergeEntry writes the owned keys of want into an existing entry, in place,
// and keeps the other keys.
func mergeEntry(current json.RawMessage, want jsonedit.Object) (json.RawMessage, error) {
	entry, err := jsonedit.Parse(current)
	if err != nil {
		return nil, err
	}
	merged := jsonedit.Object{}
	for _, member := range entry {
		if !slices.Contains(ownedKeys, member.Key) {
			merged = append(merged, member)
		} else if value, ok := want.Get(member.Key); ok {
			merged = append(merged, jsonedit.Member{Key: member.Key, Value: value})
		}
	}
	for _, member := range want {
		if _, found := merged.Get(member.Key); !found {
			merged = append(merged, member)
		}
	}
	return jsonedit.Marshal(merged)
}

// editObject rewrites one top-level key of a JSON file. change receives the
// current value (nil when absent) and returns the new one with a description
// of each change; no changes means no edit.
func editObject(path, key string, change func(json.RawMessage) (json.RawMessage, []string, error)) (*model.FileEdit, error) {
	before, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	doc := jsonedit.Object{}
	if len(bytes.TrimSpace(before)) > 0 {
		if doc, err = jsonedit.Parse(before); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	current, _ := doc.Get(key)
	value, changes, err := change(current)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(changes) == 0 {
		return nil, nil
	}
	trailingNewline := len(before) == 0 || before[len(before)-1] == '\n'
	after, err := jsonedit.Format(doc.Set(key, value), trailingNewline)
	if err != nil {
		return nil, err
	}
	return &model.FileEdit{Path: path, Before: before, After: after, Changes: changes}, nil
}
