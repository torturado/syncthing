// Package claude reads the Claude Code configuration that applies to every
// directory under the user's home: user and managed settings, the plugins they
// enable, the user and local MCP scopes in ~/.claude.json, and ~/.mcp.json.
//
// Repository .mcp.json files and project-scoped plugins are deliberately out of
// scope: they belong to the repository, not to the machine.
package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/torturado/agentcfg/internal/model"
)

const managedDir = "/etc/claude-code"

// pluginVariables are expanded by Claude Code itself for plugin files, so a
// reference to them is never an unset variable.
var pluginVariables = []string{"CLAUDE_PLUGIN_ROOT", "CLAUDE_PLUGIN_DATA"}

type mcpServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Command string            `json:"command"`
	Headers map[string]string `json:"headers"`
	Env     map[string]string `json:"env"`
}

type settings struct {
	Env                        map[string]string          `json:"env"`
	Hooks                      model.HookGroups           `json:"hooks"`
	EnabledMcpjsonServers      []string                   `json:"enabledMcpjsonServers"`
	EnableAllProjectMcpServers bool                       `json:"enableAllProjectMcpServers"`
	EnabledPlugins             map[string]json.RawMessage `json:"enabledPlugins"`
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

type installedPlugins struct {
	Plugins map[string][]struct {
		Scope       string `json:"scope"`
		InstallPath string `json:"installPath"`
	} `json:"plugins"`
}

// pluginManifest holds the manifest fields that add hooks or MCP servers.
// Each is a path, a list of paths, or the configuration inline.
type pluginManifest struct {
	Hooks      json.RawMessage `json:"hooks"`
	MCPServers json.RawMessage `json:"mcpServers"`
}

// Paths are the Claude Code files read for a home directory.
type Paths struct {
	ConfigDir string
	Settings  string
	State     string
	MCPJSON   string
	Plugins   string
}

func PathsFor(home string) Paths {
	configDir := filepath.Join(home, ".claude")
	state := filepath.Join(home, ".claude.json")
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		configDir = dir
		state = filepath.Join(dir, ".claude.json")
	}
	return Paths{
		ConfigDir: configDir,
		Settings:  filepath.Join(configDir, "settings.json"),
		State:     state,
		MCPJSON:   filepath.Join(home, ".mcp.json"),
		Plugins:   filepath.Join(configDir, "plugins", "installed_plugins.json"),
	}
}

// WatchPaths are the files whose changes can change the inventory.
func WatchPaths(home string) []string {
	p := PathsFor(home)
	return []string{p.Settings, p.State, p.MCPJSON, p.Plugins}
}

func Inventory(home string) (model.Inventory, error) {
	var inv model.Inventory
	paths := PathsFor(home)

	settingsPaths := []string{paths.Settings, filepath.Join(managedDir, "managed-settings.json")}
	dropIns, _ := filepath.Glob(filepath.Join(managedDir, "managed-settings.d", "*.json"))
	settingsPaths = append(settingsPaths, dropIns...)

	var approved []string
	approveAll := false
	// Later files take precedence, so managed settings can turn a plugin off.
	enabledPlugins := map[string]string{}
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
		for name, value := range s.EnabledPlugins {
			if string(value) == "true" {
				enabledPlugins[name] = path
			} else {
				delete(enabledPlugins, name)
			}
		}
	}

	if err := readPlugins(paths.Plugins, enabledPlugins, &inv); err != nil {
		return inv, err
	}

	var state stateFile
	if _, err := readJSON(paths.State, &state); err != nil {
		return inv, err
	}
	inv.MCPServers = append(inv.MCPServers, servers(state.MCPServers, "user", paths.State, "mcpServers", nil)...)
	for project, p := range state.Projects {
		scope := "local " + project
		inv.MCPServers = append(inv.MCPServers, servers(p.MCPServers, scope, paths.State, "projects."+project+".mcpServers", nil)...)
	}

	for _, path := range []string{paths.MCPJSON, filepath.Join(managedDir, "managed-mcp.json")} {
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

// readPlugins adds each enabled plugin with the hooks and MCP servers it
// carries. A plugin that is enabled but not installed loads nothing.
func readPlugins(registryPath string, enabled map[string]string, inv *model.Inventory) error {
	if len(enabled) == 0 {
		return nil
	}
	var registry installedPlugins
	if _, err := readJSON(registryPath, &registry); err != nil {
		return err
	}
	for _, id := range slices.Sorted(maps.Keys(enabled)) {
		inv.Plugins = append(inv.Plugins, model.Plugin{Agent: model.Claude, Name: id, Source: enabled[id]})

		installs := registry.Plugins[id]
		if len(installs) == 0 {
			continue
		}
		root := installs[0].InstallPath
		for _, install := range installs {
			if install.Scope == "user" {
				root = install.InstallPath
				break
			}
		}

		manifestPath := filepath.Join(root, ".claude-plugin", "plugin.json")
		var manifest pluginManifest
		if _, err := readJSON(manifestPath, &manifest); err != nil {
			return err
		}

		hookSources, err := pluginSources(root, manifestPath, manifest.Hooks, filepath.Join(root, "hooks", "hooks.json"))
		if err != nil {
			return err
		}
		for _, source := range hookSources {
			groups, err := pluginHooks(source.data)
			if err != nil {
				return fmt.Errorf("%s: plugin %s hooks: %w", source.path, id, err)
			}
			inv.Hooks = append(inv.Hooks, groups.Hooks(model.Claude, source.path)...)
		}

		serverSources, err := pluginSources(root, manifestPath, manifest.MCPServers, filepath.Join(root, ".mcp.json"))
		if err != nil {
			return err
		}
		pluginName, _, _ := strings.Cut(id, "@")
		for _, source := range serverSources {
			entries, err := pluginServers(source.data)
			if err != nil {
				return fmt.Errorf("%s: plugin %s MCP servers: %w", source.path, id, err)
			}
			for _, server := range servers(entries, "plugin "+id, source.path, "mcpServers", nil) {
				// Claude Code names plugin servers plugin:<plugin>:<server>.
				server.Name = "plugin:" + pluginName + ":" + server.Name
				inv.MCPServers = append(inv.MCPServers, server)
			}
		}
	}
	return nil
}

type pluginSource struct {
	path string
	data []byte
}

// pluginSources returns the default file, when present, plus the files or
// inline configuration a manifest field adds.
func pluginSources(root, manifestPath string, field json.RawMessage, defaultPath string) ([]pluginSource, error) {
	var sources []pluginSource
	add := func(path string) error {
		if slices.ContainsFunc(sources, func(s pluginSource) bool { return s.path == path }) {
			return nil
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		sources = append(sources, pluginSource{path, data})
		return nil
	}
	if err := add(defaultPath); err != nil {
		return nil, err
	}
	if len(field) == 0 || string(field) == "null" {
		return sources, nil
	}
	var single string
	var list []string
	switch {
	case json.Unmarshal(field, &single) == nil:
		list = []string{single}
	case json.Unmarshal(field, &list) == nil:
	default:
		return append(sources, pluginSource{manifestPath, field}), nil
	}
	for _, path := range list {
		if err := add(filepath.Join(root, path)); err != nil {
			return nil, err
		}
	}
	return sources, nil
}

// pluginHooks accepts both a hooks.json document ({"hooks": {...}}) and the
// bare event map a manifest can carry inline.
func pluginHooks(data []byte) (model.HookGroups, error) {
	var wrapped struct {
		Hooks model.HookGroups `json:"hooks"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Hooks != nil {
		return wrapped.Hooks, nil
	}
	var groups model.HookGroups
	err := json.Unmarshal(data, &groups)
	return groups, err
}

// pluginServers accepts both a .mcp.json document and the bare server map.
func pluginServers(data []byte) (map[string]mcpServer, error) {
	var wrapped mcpJSON
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.MCPServers != nil {
		return wrapped.MCPServers, nil
	}
	var entries map[string]mcpServer
	err := json.Unmarshal(data, &entries)
	return entries, err
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
	matches := model.EnvRef.FindAllStringSubmatch(value, -1)
	for _, match := range matches {
		if !slices.Contains(pluginVariables, match[1]) {
			f.EnvRefs = append(f.EnvRefs, match[1])
		}
	}
	f.Literal = len(matches) == 0 && value != ""
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
