// Package claude reads the Claude Code configuration that applies to every
// directory under the user's home: user and managed settings, the user and
// local MCP scopes in ~/.claude.json, and ~/.mcp.json.
//
// Repository .mcp.json files are deliberately out of scope: they belong to the
// repository, not to the machine.
package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/torturado/agentcfg/internal/model"
)

const managedDir = "/etc/claude-code"

// envRef matches ${VAR} and ${VAR:-default}, the expansion Claude Code applies
// to MCP server command, args, env, url and headers.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-[^}]*)?\}`)

type mcpServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Command string            `json:"command"`
	Headers map[string]string `json:"headers"`
	Env     map[string]string `json:"env"`
}

type settings struct {
	Env                        map[string]string `json:"env"`
	Hooks                      model.HookGroups  `json:"hooks"`
	EnabledMcpjsonServers      []string          `json:"enabledMcpjsonServers"`
	EnableAllProjectMcpServers bool              `json:"enableAllProjectMcpServers"`
}

type stateFile struct {
	MCPServers map[string]mcpServer `json:"mcpServers"`
	Projects   map[string]struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	} `json:"projects"`
}

type mcpJSON struct {
	MCPServers map[string]mcpServer `json:"mcpServers"`
}

func Inventory(home string) (model.Inventory, error) {
	var inv model.Inventory

	configDir := filepath.Join(home, ".claude")
	statePath := filepath.Join(home, ".claude.json")
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		configDir = dir
		statePath = filepath.Join(dir, ".claude.json")
	}

	settingsPaths := []string{filepath.Join(configDir, "settings.json"), filepath.Join(managedDir, "managed-settings.json")}
	dropIns, _ := filepath.Glob(filepath.Join(managedDir, "managed-settings.d", "*.json"))
	settingsPaths = append(settingsPaths, dropIns...)

	var approved []string
	approveAll := false
	for _, path := range settingsPaths {
		var s settings
		found, err := readJSON(path, &s)
		if err != nil {
			return inv, err
		}
		if !found {
			continue
		}
		inv.Hooks = append(inv.Hooks, s.Hooks.Hooks(model.Claude, path)...)
		for name, value := range s.Env {
			inv.SettingsEnv = append(inv.SettingsEnv, model.SettingsEnv{
				Agent: model.Claude, Source: path,
				Field: field(model.EnvField, name, "env."+name, value),
			})
		}
		approved = append(approved, s.EnabledMcpjsonServers...)
		approveAll = approveAll || s.EnableAllProjectMcpServers
	}

	var state stateFile
	if _, err := readJSON(statePath, &state); err != nil {
		return inv, err
	}
	inv.MCPServers = append(inv.MCPServers, servers(state.MCPServers, "user", statePath, "mcpServers", nil)...)
	for project, p := range state.Projects {
		scope := "local " + project
		inv.MCPServers = append(inv.MCPServers, servers(p.MCPServers, scope, statePath, "projects."+project+".mcpServers", nil)...)
	}

	for _, path := range []string{filepath.Join(home, ".mcp.json"), filepath.Join(managedDir, "managed-mcp.json")} {
		var file mcpJSON
		found, err := readJSON(path, &file)
		if err != nil {
			return inv, err
		}
		if !found {
			continue
		}
		if filepath.Dir(path) == managedDir {
			inv.MCPServers = append(inv.MCPServers, servers(file.MCPServers, "managed", path, "mcpServers", nil)...)
			continue
		}
		// A .mcp.json server only loads once approved in settings.
		enabled := func(name string) bool { return approveAll || slices.Contains(approved, name) }
		inv.MCPServers = append(inv.MCPServers, servers(file.MCPServers, "project ~", path, "mcpServers", enabled)...)
	}

	return inv, nil
}

func servers(entries map[string]mcpServer, scope, source, prefix string, enabled func(string) bool) []model.MCPServer {
	var result []model.MCPServer
	for name, entry := range entries {
		server := model.MCPServer{
			Agent: model.Claude, Name: name, Scope: scope, Source: source,
			URL: entry.URL, Command: entry.Command, Enabled: enabled == nil || enabled(name),
		}
		base := prefix + "." + name
		for header, value := range entry.Headers {
			server.Fields = append(server.Fields, field(model.HeaderField, header, base+".headers."+header, value))
		}
		for variable, value := range entry.Env {
			server.Fields = append(server.Fields, field(model.EnvField, variable, base+".env."+variable, value))
		}
		result = append(result, server)
	}
	return result
}

// field classifies a value without keeping it: a value with no ${VAR}
// reference is written in the file.
func field(kind model.FieldKind, name, path, value string) model.Field {
	f := model.Field{Kind: kind, Name: name, Path: path}
	for _, match := range envRef.FindAllStringSubmatch(value, -1) {
		f.EnvRefs = append(f.EnvRefs, match[1])
	}
	f.Literal = len(f.EnvRefs) == 0 && value != ""
	return f
}

func readJSON(path string, target any) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}
