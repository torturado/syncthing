// Package spec loads the desired state: the MCP servers every machine must
// have and the only hooks allowed to exist.
package spec

import (
	"fmt"
	"slices"

	"github.com/BurntSushi/toml"

	"github.com/torturado/agentcfg/internal/model"
)

type Server struct {
	URL     string `toml:"url"`
	Command string `toml:"command"`
	// Agents that must have the server. Empty means every supported agent.
	Agents []model.Agent `toml:"agents"`
}

type Spec struct {
	MCPServers map[string]Server `toml:"mcp_servers"`
	// Hooks lists, per agent, the hook commands allowed to exist. Any other
	// hook is reported; an agent without an entry allows none.
	Hooks map[model.Agent][]string `toml:"hooks"`
}

func Load(path string) (Spec, error) {
	var s Spec
	meta, err := toml.DecodeFile(path, &s)
	if err != nil {
		return s, err
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return s, fmt.Errorf("%s: unknown keys %v", path, undecoded)
	}
	for name, server := range s.MCPServers {
		if server.URL == "" && server.Command == "" {
			return s, fmt.Errorf("%s: mcp_servers.%s needs url or command", path, name)
		}
		for _, agent := range server.Agents {
			if !slices.Contains(model.Agents, agent) {
				return s, fmt.Errorf("%s: mcp_servers.%s: unknown agent %q", path, name, agent)
			}
		}
	}
	for agent := range s.Hooks {
		if !slices.Contains(model.Agents, agent) {
			return s, fmt.Errorf("%s: hooks: unknown agent %q", path, agent)
		}
	}
	return s, nil
}

func (s Server) AppliesTo(agent model.Agent) bool {
	return len(s.Agents) == 0 || slices.Contains(s.Agents, agent)
}
