// Package model holds the agent-neutral view of what is configured on a machine.
//
// Secret values are never stored: a field only records whether its value is
// written in the file and which environment variables it reads.
package model

import (
	"crypto/sha256"
	"regexp"
	"strings"
)

type Agent string

const (
	Claude Agent = "claude"
	Codex  Agent = "codex"
)

// Agents lists every supported agent in report order.
var Agents = []Agent{Claude, Codex}

// SecretName matches header and variable names that carry credentials.
var SecretName = regexp.MustCompile(`(?i)authorization|token|secret|passw|api[-_]?key|cookie|credential|private[-_]?key|(^|[-_])key$`)

// EnvRef matches ${VAR} and ${VAR:-default}, the expansion Claude Code applies
// to MCP server command, args, env, url and headers.
var EnvRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-[^}]*)?\}`)

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
	// Sum is the SHA-256 of the value as the spec would write it, such as
	// "Bearer ${TOKEN}" for a Codex bearer_token_env_var, so it can be
	// compared with the spec without being kept.
	Sum [sha256.Size]byte
}

func ValueSum(value string) [sha256.Size]byte { return sha256.Sum256([]byte(value)) }

type MCPServer struct {
	Agent   Agent
	Name    string
	Scope   string
	Source  string
	URL     string
	Command string
	Args    []string
	Enabled bool
	Fields  []Field
}

type Hook struct {
	Agent   Agent
	Source  string
	Event   string
	Command string
}

// Plugin is a Claude Code plugin enabled in a machine-wide settings file.
type Plugin struct {
	Agent  Agent
	Name   string
	Source string
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
	Plugins     []Plugin
	SettingsEnv []SettingsEnv
}

func (inv *Inventory) Merge(other Inventory) {
	inv.MCPServers = append(inv.MCPServers, other.MCPServers...)
	inv.Hooks = append(inv.Hooks, other.Hooks...)
	inv.Plugins = append(inv.Plugins, other.Plugins...)
	inv.SettingsEnv = append(inv.SettingsEnv, other.SettingsEnv...)
}

// HookHandler is one hook command. Claude Code also accepts the arguments as
// a separate list.
type HookHandler struct {
	Type    string   `json:"type" toml:"type"`
	Command string   `json:"command" toml:"command"`
	Args    []string `json:"args" toml:"args"`
}

// HookCommand is how a hook is named in reports and in the spec: the command
// followed by its arguments.
func (h HookHandler) HookCommand() string {
	return strings.Join(append([]string{h.Command}, h.Args...), " ")
}

// HookGroups is the hooks shape shared by Claude Code settings, Claude Code
// plugins and Codex hooks.json: event name → matcher groups → handlers.
type HookGroups map[string][]struct {
	Hooks []HookHandler `json:"hooks" toml:"hooks"`
}

func (groups HookGroups) Hooks(agent Agent, source string) []Hook {
	var hooks []Hook
	for event, matchers := range groups {
		for _, matcher := range matchers {
			for _, handler := range matcher.Hooks {
				hooks = append(hooks, Hook{Agent: agent, Source: source, Event: event, Command: handler.HookCommand()})
			}
		}
	}
	return hooks
}

// FileEdit is a change to one configuration file that apply would write.
type FileEdit struct {
	Path string
	// Before is the content the edit was computed from; nil when the file
	// does not exist yet.
	Before  []byte
	After   []byte
	Changes []string
}
