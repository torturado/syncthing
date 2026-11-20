// Package codex reads the Codex CLI configuration in $CODEX_HOME (default
// ~/.codex) and the system file /etc/codex/config.toml.
package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/torturado/agentcfg/internal/model"
)

const systemConfig = "/etc/codex/config.toml"

type mcpServer struct {
	URL               string            `toml:"url"`
	Command           string            `toml:"command"`
	Args              []string          `toml:"args"`
	Enabled           *bool             `toml:"enabled"`
	HTTPHeaders       map[string]string `toml:"http_headers"`
	EnvHTTPHeaders    map[string]string `toml:"env_http_headers"`
	BearerToken       string            `toml:"bearer_token"`
	BearerTokenEnvVar string            `toml:"bearer_token_env_var"`
	Env               map[string]string `toml:"env"`
	EnvVars           []string          `toml:"env_vars"`
}

type config struct {
	MCPServers map[string]mcpServer `toml:"mcp_servers"`
	// Hooks holds inline hook events plus "state", where Codex stores the
	// trust hash of each reviewed hook.
	Hooks map[string]toml.Primitive `toml:"hooks"`
}

type hooksFile struct {
	Hooks model.HookGroups `json:"hooks"`
}

func Inventory(home string) (model.Inventory, error) {
	var inv model.Inventory

	codexHome := codexHome(home)

	for _, entry := range []struct{ path, scope string }{
		{filepath.Join(codexHome, "config.toml"), "user"},
		{systemConfig, "system"},
	} {
		if err := readConfig(entry.path, entry.scope, &inv); err != nil {
			return inv, err
		}
	}

	hooksPath := filepath.Join(codexHome, "hooks.json")
	data, err := os.ReadFile(hooksPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return inv, err
	default:
		var file hooksFile
		if err := json.Unmarshal(data, &file); err != nil {
			return inv, fmt.Errorf("%s: %w", hooksPath, err)
		}
		inv.Hooks = append(inv.Hooks, file.Hooks.Hooks(model.Codex, hooksPath)...)
	}

	return inv, nil
}

func codexHome(home string) string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".codex")
}

// WatchPaths are the files whose changes can change the inventory.
func WatchPaths(home string) []string {
	return []string{filepath.Join(codexHome(home), "config.toml"), filepath.Join(codexHome(home), "hooks.json")}
}

func readConfig(path, scope string, inv *model.Inventory) error {
	var cfg config
	meta, err := toml.DecodeFile(path, &cfg)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	for event, raw := range cfg.Hooks {
		if event == "state" {
			continue
		}
		var matchers []struct {
			Hooks []struct {
				Command string `toml:"command"`
			} `toml:"hooks"`
		}
		// An event whose shape is not understood is still reported, so an
		// unknown hook never passes unnoticed.
		if err := meta.PrimitiveDecode(raw, &matchers); err != nil {
			inv.Hooks = append(inv.Hooks, model.Hook{Agent: model.Codex, Source: path, Event: event, Command: "(unreadable hooks." + event + ")"})
			continue
		}
		for _, matcher := range matchers {
			for _, handler := range matcher.Hooks {
				inv.Hooks = append(inv.Hooks, model.Hook{Agent: model.Codex, Source: path, Event: event, Command: handler.Command})
			}
		}
	}

	for name, entry := range cfg.MCPServers {
		server := model.MCPServer{
			Agent: model.Codex, Name: name, Scope: scope, Source: path,
			URL: entry.URL, Command: entry.Command, Args: entry.Args, Enabled: entry.Enabled == nil || *entry.Enabled,
		}
		base := "mcp_servers." + name
		// Each field carries the value the spec would write for it.
		add := func(kind model.FieldKind, name, path, value string, literal bool, refs ...string) {
			server.Fields = append(server.Fields, model.Field{Kind: kind, Name: name, Path: path, Literal: literal, EnvRefs: refs, Sum: model.ValueSum(value)})
		}
		for header, value := range entry.HTTPHeaders {
			add(model.HeaderField, header, base+".http_headers."+header, value, value != "")
		}
		for header, variable := range entry.EnvHTTPHeaders {
			add(model.HeaderField, header, base+".env_http_headers."+header, "${"+variable+"}", false, variable)
		}
		if entry.BearerToken != "" {
			add(model.TokenField, "Authorization", base+".bearer_token", "Bearer "+entry.BearerToken, true)
		}
		if entry.BearerTokenEnvVar != "" {
			add(model.TokenField, "Authorization", base+".bearer_token_env_var", "Bearer ${"+entry.BearerTokenEnvVar+"}", false, entry.BearerTokenEnvVar)
		}
		for variable, value := range entry.Env {
			add(model.EnvField, variable, base+".env."+variable, value, value != "")
		}
		for _, variable := range entry.EnvVars {
			// Codex gives an explicit env value precedence over a passthrough.
			if _, set := entry.Env[variable]; set {
				continue
			}
			add(model.EnvField, variable, base+".env_vars", "${"+variable+"}", false, variable)
		}
		inv.MCPServers = append(inv.MCPServers, server)
	}
	return nil
}
