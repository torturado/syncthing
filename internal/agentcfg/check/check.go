// Package check compares a machine's inventory with the spec.
package check

import (
	"fmt"
	"os"
	"slices"
	"sort"

	"github.com/torturado/agentcfg/internal/model"
	"github.com/torturado/agentcfg/internal/spec"
)

type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
)

// Finding is one deviation from the spec. Source is the file it was read
// from and Location the key inside it.
type Finding struct {
	Severity Severity    `json:"severity"`
	Agent    model.Agent `json:"agent"`
	Rule     string      `json:"rule"`
	Source   string      `json:"source,omitempty"`
	Location string      `json:"location"`
	Message  string      `json:"message"`
}

func Run(desired spec.Spec, inv model.Inventory) []Finding {
	var findings []Finding

	for _, agent := range model.Agents {
		for name, want := range desired.MCPServers {
			if !want.AppliesTo(agent) {
				continue
			}
			var found []model.MCPServer
			for _, server := range inv.MCPServers {
				if server.Agent == agent && server.Name == name && server.Enabled {
					found = append(found, server)
				}
			}
			if len(found) == 0 {
				findings = append(findings, Finding{Error, agent, "missing-server", "", "mcp_servers." + name, "declared in the spec but not configured or not enabled"})
				continue
			}
			if len(found) > 1 {
				findings = append(findings, Finding{Warning, agent, "shadowed-server", found[0].Source, name, fmt.Sprintf("defined in %d scopes; only one applies", len(found))})
			}
			for _, server := range found {
				if want.URL != "" && server.URL != want.URL {
					findings = append(findings, Finding{Error, agent, "server-mismatch", server.Source, server.Name, fmt.Sprintf("url is %q, spec says %q", server.URL, want.URL)})
				}
				if want.Command != "" && server.Command != want.Command {
					findings = append(findings, Finding{Error, agent, "server-mismatch", server.Source, server.Name, fmt.Sprintf("command is %q, spec says %q", server.Command, want.Command)})
				}
			}
		}
	}

	for _, server := range inv.MCPServers {
		want, declared := desired.MCPServers[server.Name]
		if server.Enabled && (!declared || !want.AppliesTo(server.Agent)) {
			findings = append(findings, Finding{Error, server.Agent, "undeclared-server", server.Source, server.Name, "enabled in scope " + server.Scope + " but not in the spec"})
		}
		for _, field := range server.Fields {
			findings = append(findings, fieldFindings(server.Agent, server.Source, field, server.Enabled)...)
		}
	}

	for _, env := range inv.SettingsEnv {
		// Claude Code sets settings env values as written: a reference
		// reaches the agent as the literal text ${NAME}.
		if len(env.Field.EnvRefs) > 0 {
			findings = append(findings, Finding{Error, env.Agent, "unexpanded-variable", env.Source, env.Field.Path, "settings env values are not expanded; set the variable in the environment instead, or use apiKeyHelper for a credential"})
			continue
		}
		findings = append(findings, fieldFindings(env.Agent, env.Source, env.Field, true)...)
	}

	for _, plugin := range inv.Plugins {
		if !slices.Contains(desired.Plugins[plugin.Agent], plugin.Name) {
			findings = append(findings, Finding{Error, plugin.Agent, "undeclared-plugin", plugin.Source, "enabledPlugins." + plugin.Name, "enabled but not in the spec"})
		}
	}

	for _, hook := range inv.Hooks {
		if !slices.Contains(desired.Hooks[hook.Agent], hook.Command) {
			findings = append(findings, Finding{Error, hook.Agent, "undeclared-hook", hook.Source, hook.Event, hook.Command})
		}
	}

	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		if a.Severity != b.Severity {
			return a.Severity == Error
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.Location < b.Location
	})
	return findings
}

func fieldFindings(agent model.Agent, source string, field model.Field, enabled bool) []Finding {
	var findings []Finding
	if field.Literal && (field.Kind == model.TokenField || model.SecretName.MatchString(field.Name)) {
		findings = append(findings, Finding{Error, agent, "literal-secret", source, field.Path, "credential written in the file; reference an environment variable instead"})
	}
	if enabled {
		for _, variable := range field.EnvRefs {
			if _, set := os.LookupEnv(variable); !set {
				findings = append(findings, Finding{Warning, agent, "unset-variable", source, field.Path, variable + " is not set in this environment"})
			}
		}
	}
	return findings
}
