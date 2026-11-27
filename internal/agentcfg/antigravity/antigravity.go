// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Package antigravity reads the global configuration that Antigravity (the
// app, the IDE and the agy CLI) shares in ~/.gemini/config: the MCP servers
// in mcp_config.json, the hooks in hooks.json, and the plugins installed in
// plugins/ with the hooks and MCP servers they carry.
//
// Workspace .agents/ directories are out of scope: they belong to the
// repository, not to the machine.
package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/syncthing/syncthing/internal/agentcfg/model"
)

// Shared are the entries of ~/.gemini that are configuration a fleet can
// share: the global rules and what Antigravity reads from config/. Not
// config/mcp_config.json, into which apply writes credentials, nor what
// belongs to one machine: config/config.json with its per-machine settings,
// config/projects, and the antigravity* directories with conversations,
// caches and installation IDs.
var Shared = []string{
	"GEMINI.md",
	"config/agents", "config/global_workflows", "config/hooks.json",
	"config/plugins", "config/skills",
}

// stateDirs are where the Antigravity app, CLI and IDE keep their state.
// Only an installed Antigravity creates them, and they are not synced.
var stateDirs = []string{"antigravity", "antigravity-cli", "antigravity-ide"}

// Paths are the Antigravity files read for a home directory.
type Paths struct {
	// Dir is ~/.gemini, the directory Antigravity shares with Gemini CLI.
	Dir       string
	MCPConfig string
	Hooks     string
	Plugins   string
	// GatewayEnv is the environment.d file with the AGY_LLM_GATEWAY_*
	// variables, which systemd passes to the user's session and units.
	GatewayEnv string
}

func PathsFor(home string) Paths {
	dir := filepath.Join(home, ".gemini")
	config := filepath.Join(dir, "config")
	return Paths{
		Dir:        dir,
		MCPConfig:  filepath.Join(config, "mcp_config.json"),
		Hooks:      filepath.Join(config, "hooks.json"),
		Plugins:    filepath.Join(config, "plugins"),
		GatewayEnv: filepath.Join(home, ".config", "environment.d", "agentcfg-antigravity.conf"),
	}
}

// Installed reports whether Antigravity has run on the machine.
func Installed(home string) bool {
	dir := PathsFor(home).Dir
	return slices.ContainsFunc(stateDirs, func(name string) bool {
		info, err := os.Stat(filepath.Join(dir, name))
		return err == nil && info.IsDir()
	})
}

// WatchPaths are the files whose changes can change the inventory or the
// gateway environment.
func WatchPaths(home string) []string {
	p := PathsFor(home)
	return []string{p.MCPConfig, p.Hooks, p.GatewayEnv}
}

type mcpServer struct {
	ServerURL string            `json:"serverUrl"`
	URL       string            `json:"url"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	Headers   map[string]string `json:"headers"`
	Disabled  bool              `json:"disabled"`
}

type mcpConfig struct {
	MCPServers map[string]mcpServer `json:"mcpServers"`
}

// hookEntry is an event's entry: a matcher group, or a handler itself.
type hookEntry struct {
	model.HookHandler

	Hooks []model.HookHandler `json:"hooks"`
}

func Inventory(home string) (model.Inventory, error) {
	var inv model.Inventory
	if !Installed(home) {
		inv.Absent = []model.Agent{model.Antigravity}
		return inv, nil
	}
	paths := PathsFor(home)
	if err := readServers(paths.MCPConfig, "user", "", &inv); err != nil {
		return inv, err
	}
	if err := readHooks(paths.Hooks, &inv); err != nil {
		return inv, err
	}

	entries, err := os.ReadDir(paths.Plugins)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return inv, err
	}
	for _, entry := range entries {
		root := filepath.Join(paths.Plugins, entry.Name())
		if _, err := os.Stat(filepath.Join(root, "plugin.json")); err != nil {
			continue
		}
		inv.Plugins = append(inv.Plugins, model.Plugin{Agent: model.Antigravity, Name: entry.Name(), Source: root})
		if err := readServers(filepath.Join(root, "mcp_config.json"), "plugin "+entry.Name(), "plugin:"+entry.Name()+":", &inv); err != nil {
			return inv, err
		}
		if err := readHooks(filepath.Join(root, "hooks.json"), &inv); err != nil {
			return inv, err
		}
	}
	return inv, nil
}

// readServers adds the servers of an mcp_config.json. A plugin's servers are
// named plugin:<plugin>:<server>, so the spec can tell them apart.
func readServers(path, scope, namePrefix string, inv *model.Inventory) error {
	var file mcpConfig
	if err := readJSON(path, &file); err != nil {
		return err
	}
	for name, entry := range file.MCPServers {
		url := entry.ServerURL
		if url == "" {
			url = entry.URL
		}
		server := model.MCPServer{
			Agent: model.Antigravity, Name: namePrefix + name, Scope: scope, Source: path,
			URL: url, Command: entry.Command, Args: entry.Args, Enabled: !entry.Disabled,
		}
		base := "mcpServers." + name
		for header, value := range entry.Headers {
			server.Fields = append(server.Fields, model.NewField(model.HeaderField, header, base+".headers."+header, value))
		}
		for variable, value := range entry.Env {
			server.Fields = append(server.Fields, model.NewField(model.EnvField, variable, base+".env."+variable, value))
		}
		inv.MCPServers = append(inv.MCPServers, server)
	}
	return nil
}

// readHooks adds the hooks of a hooks.json, except those disabled.
func readHooks(path string, inv *model.Inventory) error {
	var file map[string]map[string]json.RawMessage
	if err := readJSON(path, &file); err != nil {
		return err
	}
	for name, hook := range file {
		if enabled, ok := hook["enabled"]; ok && string(bytes.TrimSpace(enabled)) == "false" {
			continue
		}
		for event, raw := range hook {
			if event == "enabled" {
				continue
			}
			var entries []hookEntry
			if err := json.Unmarshal(raw, &entries); err != nil {
				return fmt.Errorf("%s: %s.%s: %w", path, name, event, err)
			}
			for _, entry := range entries {
				handlers := entry.Hooks
				if entry.Hooks == nil && entry.Command != "" {
					handlers = []model.HookHandler{entry.HookHandler}
				}
				for _, handler := range handlers {
					inv.Hooks = append(inv.Hooks, model.Hook{Agent: model.Antigravity, Source: path, Event: event, Command: handler.HookCommand()})
				}
			}
		}
	}
	return nil
}

// readJSON decodes path into target; a missing or empty file, which
// Antigravity creates, holds nothing.
func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) || err == nil && len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
