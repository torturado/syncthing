package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/torturado/agentcfg/internal/jsonedit"
	"github.com/torturado/agentcfg/internal/model"
	"github.com/torturado/agentcfg/internal/spec"
)

// Plan computes the edits that bring Codex to the spec. In config.toml it owns
// the mcp_servers tables and the inline hook events, never projects or the
// hooks.state trust store; in hooks.json it owns the hooks. Only the tables it
// changes are rewritten: comments and layout elsewhere are kept.
func Plan(home string, desired spec.Spec) ([]model.FileEdit, error) {
	codexHome := codexHome(home)
	allowed := func(command string) bool { return slices.Contains(desired.Hooks[model.Codex], command) }
	var edits []model.FileEdit

	configPath := filepath.Join(codexHome, "config.toml")
	edit, err := planConfig(configPath, desired, allowed)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", configPath, err)
	}
	if edit != nil {
		edits = append(edits, *edit)
	}

	hooksPath := filepath.Join(codexHome, "hooks.json")
	before, err := os.ReadFile(hooksPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if len(bytes.TrimSpace(before)) > 0 {
		doc, err := jsonedit.Parse(before)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", hooksPath, err)
		}
		if current, ok := doc.Get("hooks"); ok {
			value, removed, err := jsonedit.FilterHooks(current, allowed)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", hooksPath, err)
			}
			if len(removed) > 0 {
				after, err := jsonedit.Format(doc.Set("hooks", value), before[len(before)-1] == '\n')
				if err != nil {
					return nil, err
				}
				edits = append(edits, model.FileEdit{Path: hooksPath, Before: before, After: after, Changes: prefix("remove hook ", removed)})
			}
		}
	}
	return edits, nil
}

func planConfig(path string, desired spec.Spec, allowed func(string) bool) (*model.FileEdit, error) {
	before, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var doc map[string]any
	if _, err := toml.Decode(string(before), &doc); err != nil {
		return nil, err
	}

	want, err := renderServers(desired)
	if err != nil {
		return nil, err
	}

	var changes []string
	expected := maps.Clone(doc)
	text := string(before)

	current, _ := doc["mcp_servers"].(map[string]any)
	if serverChanges, rewrite := diffServers(current, want); len(serverChanges) > 0 {
		changes = append(changes, serverChanges...)
		var replacement strings.Builder
		expectedServers := maps.Clone(current)
		if expectedServers == nil {
			expectedServers = map[string]any{}
		}
		for _, name := range rewrite {
			delete(expectedServers, name)
			if server, declared := want[name]; declared {
				if replacement.Len() > 0 {
					replacement.WriteString("\n")
				}
				replacement.WriteString(server.text)
				expectedServers[name] = server.decoded
			}
		}
		text = replaceTables(text, func(key []string) bool {
			return len(key) > 1 && key[0] == "mcp_servers" && slices.Contains(rewrite, key[1])
		}, replacement.String())
		setOrDelete(expected, "mcp_servers", expectedServers)
	}

	hooks, _ := doc["hooks"].(map[string]any)
	expectedHooks := maps.Clone(hooks)
	for _, event := range slices.Sorted(maps.Keys(hooks)) {
		if event == "state" {
			continue
		}
		kept, removed, err := filterEvent(hooks[event], allowed)
		if err != nil {
			return nil, fmt.Errorf("hooks.%s: %w", event, err)
		}
		if len(removed) == 0 {
			continue
		}
		for _, command := range removed {
			changes = append(changes, "remove hook "+event+": "+command)
		}
		var replacement string
		if len(kept) > 0 {
			if replacement, err = encodeTables(map[string]any{"hooks": map[string]any{event: kept}}); err != nil {
				return nil, err
			}
		}
		text = replaceTables(text, func(key []string) bool { return len(key) > 1 && key[0] == "hooks" && key[1] == event }, replacement)
		var decoded map[string]any
		if _, err := toml.Decode(replacement, &decoded); err != nil {
			return nil, err
		}
		decodedHooks, _ := decoded["hooks"].(map[string]any)
		setOrDelete(expectedHooks, event, decodedHooks[event])
	}
	if len(changes) == 0 {
		return nil, nil
	}
	if hooks != nil {
		setOrDelete(expected, "hooks", expectedHooks)
	}

	// The rewrite is done on text to keep comments, so check that it changed
	// exactly the owned keys. A file that defines them in a form the rewrite
	// does not handle, such as dotted keys at the top level, is refused.
	var result map[string]any
	if _, err := toml.Decode(text, &result); err != nil {
		return nil, fmt.Errorf("rewrite produced invalid TOML: %w", err)
	}
	if !reflect.DeepEqual(normalize(result), normalize(expected)) {
		return nil, fmt.Errorf("mcp_servers or hooks are written in a form apply cannot rewrite safely; move them into [mcp_servers.<name>] and [[hooks.<event>]] tables")
	}
	return &model.FileEdit{Path: path, Before: before, After: []byte(text), Changes: changes}, nil
}

type renderedServer struct {
	decoded any
	text    string
}

// renderServers returns the Codex servers declared for this machine as TOML
// tables, and decoded for comparison with the file.
func renderServers(desired spec.Spec) (map[string]renderedServer, error) {
	servers := map[string]renderedServer{}
	for name, server := range desired.MCPServers {
		if !server.AppliesTo(model.Codex) {
			continue
		}
		rendered, err := spec.CodexServer(server)
		if err != nil {
			return nil, fmt.Errorf("mcp_servers.%s: %w", name, err)
		}
		text := serverTable(name, rendered)
		var decoded map[string]any
		if _, err := toml.Decode(text, &decoded); err != nil {
			return nil, err
		}
		servers[name] = renderedServer{decoded["mcp_servers"].(map[string]any)[name], text}
	}
	return servers, nil
}

// serverTable renders one server the way the Codex documentation writes it,
// with headers and environment as inline tables.
func serverTable(name string, c spec.CodexMCPServer) string {
	var b strings.Builder
	b.WriteString("[mcp_servers." + tomlKey(name) + "]\n")
	line := func(key, value string) {
		b.WriteString(key + " = " + value + "\n")
	}
	if c.URL != "" {
		line("url", tomlString(c.URL))
	}
	if len(c.HTTPHeaders) > 0 {
		line("http_headers", inlineTable(c.HTTPHeaders))
	}
	if len(c.EnvHTTPHeaders) > 0 {
		line("env_http_headers", inlineTable(c.EnvHTTPHeaders))
	}
	if c.BearerTokenEnvVar != "" {
		line("bearer_token_env_var", tomlString(c.BearerTokenEnvVar))
	}
	if c.Command != "" {
		line("command", tomlString(c.Command))
	}
	if len(c.Args) > 0 {
		line("args", stringArray(c.Args))
	}
	if len(c.Env) > 0 {
		line("env", inlineTable(c.Env))
	}
	if len(c.EnvVars) > 0 {
		line("env_vars", stringArray(c.EnvVars))
	}
	return b.String()
}

func inlineTable(m map[string]string) string {
	var parts []string
	for _, key := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, tomlKey(key)+" = "+tomlString(m[key]))
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

func stringArray(items []string) string {
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = tomlString(item)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// tomlString writes a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\u%04X", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// diffServers describes the changes and names the servers whose tables must
// be rewritten. A server that is not declared but disabled is left alone, as
// check does.
func diffServers(current map[string]any, want map[string]renderedServer) ([]string, []string) {
	var changes, rewrite []string
	for _, name := range slices.Sorted(maps.Keys(current)) {
		if _, declared := want[name]; declared {
			continue
		}
		if entry, _ := current[name].(map[string]any); entry["enabled"] == false {
			continue
		}
		changes = append(changes, "remove MCP server "+name)
		rewrite = append(rewrite, name)
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		existing, found := current[name]
		switch {
		case !found:
			changes = append(changes, "add MCP server "+name)
		case !reflect.DeepEqual(existing, want[name].decoded):
			changes = append(changes, "update MCP server "+name)
		default:
			continue
		}
		rewrite = append(rewrite, name)
	}
	return changes, rewrite
}

// filterEvent drops the handlers of one inline hook event that are not
// allowed, and the matcher groups left without handlers.
func filterEvent(raw any, allowed func(string) bool) ([]map[string]any, []string, error) {
	groups, ok := raw.([]map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("not an array of tables")
	}
	var kept []map[string]any
	var removed []string
	for _, group := range groups {
		handlers, _ := group["hooks"].([]map[string]any)
		var keptHandlers []map[string]any
		for _, handler := range handlers {
			command, _ := handler["command"].(string)
			if allowed(command) {
				keptHandlers = append(keptHandlers, handler)
			} else {
				removed = append(removed, command)
			}
		}
		if len(keptHandlers) == 0 {
			continue
		}
		group = maps.Clone(group)
		group["hooks"] = keptHandlers
		kept = append(kept, group)
	}
	return kept, removed, nil
}

var tableHeader = regexp.MustCompile(`^\s*\[\[?\s*(.+?)\s*\]\]?\s*(#.*)?$`)

// replaceTables removes every table whose key matches and puts replacement
// where the first of them was, or at the end of the file. Comment lines just
// before the next table stay, since they usually describe that table.
func replaceTables(text string, match func(key []string) bool, replacement string) string {
	var out, pending []string
	insertAt := -1
	removing := false
	for _, line := range strings.SplitAfter(text, "\n") {
		if line == "" {
			continue
		}
		if m := tableHeader.FindStringSubmatch(line); m != nil {
			if match(splitKey(m[1])) {
				if insertAt < 0 {
					insertAt = len(out)
				}
				removing = true
				pending = nil
				continue
			}
			if removing {
				out = appendKept(out, pending)
				pending = nil
				removing = false
			}
		}
		if !removing {
			out = append(out, line)
			continue
		}
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			pending = append(pending, line)
		} else {
			pending = nil
		}
	}
	out = appendKept(out, pending)
	if replacement == "" {
		return strings.Join(out, "")
	}
	block := strings.TrimRight(replacement, "\n") + "\n"
	if insertAt < 0 {
		body := strings.TrimRight(strings.Join(out, ""), "\n")
		if body == "" {
			return block
		}
		return body + "\n\n" + block
	}
	if insertAt < len(out) && strings.TrimSpace(out[insertAt]) != "" {
		block += "\n"
	}
	return strings.Join(slices.Insert(out, insertAt, block), "")
}

// appendKept adds the comment and blank lines kept from a removed table,
// without doubling the blank line that already ends out.
func appendKept(out, kept []string) []string {
	for len(kept) > 0 && strings.TrimSpace(kept[0]) == "" && (len(out) == 0 || strings.TrimSpace(out[len(out)-1]) == "") {
		kept = kept[1:]
	}
	return append(out, kept...)
}

// splitKey splits a dotted table key, honoring quoted parts.
func splitKey(key string) []string {
	var parts []string
	var current strings.Builder
	var quote rune
	for _, r := range key {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			current.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
		case r == '.':
			parts = append(parts, strings.TrimSpace(current.String()))
			current.Reset()
		case r == ' ' || r == '\t':
		default:
			current.WriteRune(r)
		}
	}
	return append(parts, strings.TrimSpace(current.String()))
}

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func tomlKey(key string) string {
	if bareKey.MatchString(key) {
		return key
	}
	return tomlString(key)
}

func encodeTables(value map[string]any) (string, error) {
	var buf strings.Builder
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(value); err != nil {
		return "", err
	}
	// The encoder opens every parent table; an empty [hooks] header would be
	// redundant next to the existing one.
	text := strings.TrimPrefix(buf.String(), "[hooks]\n")
	return strings.TrimLeft(text, "\n"), nil
}

func setOrDelete(m map[string]any, key string, value any) {
	if m == nil {
		return
	}
	empty := value == nil
	switch v := value.(type) {
	case map[string]any:
		empty = len(v) == 0
	case []map[string]any:
		empty = len(v) == 0
	}
	if empty {
		delete(m, key)
	} else {
		m[key] = value
	}
}

// normalize makes decoded documents comparable: empty tables equal missing
// ones, and every value has the type a JSON round trip gives it.
func normalize(doc map[string]any) any {
	for key, value := range doc {
		if v, ok := value.(map[string]any); ok && len(v) == 0 {
			delete(doc, key)
		}
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return doc
	}
	var out any
	_ = json.Unmarshal(data, &out)
	return out
}

func prefix(p string, items []string) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = p + item
	}
	return out
}
