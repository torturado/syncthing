// Package model holds the agent-neutral view of what is configured on a machine.
//
// Secret values are never stored: a field only records whether its value is
// written in the file and which environment variables it reads.
package model

type Agent string

const (
	Claude Agent = "claude"
	Codex  Agent = "codex"
)

// Agents lists every supported agent in report order.
var Agents = []Agent{Claude, Codex}

type FieldKind string

const (
	HeaderField FieldKind = "header"
	EnvField    FieldKind = "env"
	TokenField  FieldKind = "token"
)

// Field is a header, environment variable or token that an MCP server or a
// settings file passes on. Path is the location inside its file, for reports.
type Field struct {
	Kind    FieldKind
	Name    string
	Path    string
	Literal bool
	EnvRefs []string
}

type MCPServer struct {
	Agent   Agent
	Name    string
	Scope   string
	Source  string
	URL     string
	Command string
	Enabled bool
	Fields  []Field
}

type Hook struct {
	Agent   Agent
	Source  string
	Event   string
	Command string
}

// SettingsEnv is an environment variable set by an agent's settings file.
type SettingsEnv struct {
	Agent  Agent
	Source string
	Field  Field
}

type Inventory struct {
	MCPServers  []MCPServer
	Hooks       []Hook
	SettingsEnv []SettingsEnv
}

func (inv *Inventory) Merge(other Inventory) {
	inv.MCPServers = append(inv.MCPServers, other.MCPServers...)
	inv.Hooks = append(inv.Hooks, other.Hooks...)
	inv.SettingsEnv = append(inv.SettingsEnv, other.SettingsEnv...)
}

// HookGroups is the hooks shape shared by Claude Code settings and Codex
// hooks.json: event name → matcher groups → handlers.
type HookGroups map[string][]struct {
	Hooks []struct {
		Type    string `json:"type" toml:"type"`
		Command string `json:"command" toml:"command"`
	} `json:"hooks" toml:"hooks"`
}

func (groups HookGroups) Hooks(agent Agent, source string) []Hook {
	var hooks []Hook
	for event, matchers := range groups {
		for _, matcher := range matchers {
			for _, handler := range matcher.Hooks {
				hooks = append(hooks, Hook{Agent: agent, Source: source, Event: event, Command: handler.Command})
			}
		}
	}
	return hooks
}
