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

// Plan computes the edits that bring Claude Code to the spec. It owns only
// mcpServers in ~/.claude.json and the hooks in the user settings file; project
// entries, permissions and every other key are carried over unchanged.
//
// A declared server already enabled in ~/.mcp.json or in managed settings with
// the declared target is left there rather than duplicated in the user scope.
func Plan(home string, desired spec.Spec, inv model.Inventory) ([]model.FileEdit, error) {
	paths := PathsFor(home)
	var edits []model.FileEdit

	want := map[string]json.RawMessage{}
	for name, server := range desired.MCPServers {
		if !server.AppliesTo(model.Claude) || providedElsewhere(name, server, inv) {
			continue
		}
		entry, err := render(server)
		if err != nil {
			return nil, err
		}
		raw, err := jsonedit.Marshal(entry)
		if err != nil {
			return nil, err
		}
		want[name] = raw
	}
	edit, err := editObject(paths.State, "mcpServers", func(current json.RawMessage) (json.RawMessage, []string, error) {
		return reconcileServers(current, want)
	})
	if err != nil {
		return nil, err
	}
	if edit != nil {
		edits = append(edits, *edit)
	}

	edit, err = editObject(paths.Settings, "hooks", func(current json.RawMessage) (json.RawMessage, []string, error) {
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

func providedElsewhere(name string, want spec.Server, inv model.Inventory) bool {
	return slices.ContainsFunc(inv.MCPServers, func(s model.MCPServer) bool {
		return s.Agent == model.Claude && s.Name == name && s.Enabled &&
			(s.Scope == "managed" || s.Scope == "project ~") &&
			s.URL == want.URL && s.Command == want.Command
	})
}

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

// reconcileServers keeps the order of servers that stay, replaces those that
// differ and appends new ones in name order.
func reconcileServers(current json.RawMessage, want map[string]json.RawMessage) (json.RawMessage, []string, error) {
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
		value, declared := want[member.Key]
		switch {
		case !declared:
			changes = append(changes, "remove MCP server "+member.Key)
		case jsonedit.Equal(member.Value, value):
			next = append(next, member)
		default:
			changes = append(changes, "update MCP server "+member.Key)
			next = append(next, jsonedit.Member{Key: member.Key, Value: value})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if _, found := existing.Get(name); !found {
			changes = append(changes, "add MCP server "+name)
			next = append(next, jsonedit.Member{Key: name, Value: want[name]})
		}
	}
	if len(changes) == 0 {
		return current, nil, nil
	}
	value, err := jsonedit.Marshal(next)
	return value, changes, err
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
