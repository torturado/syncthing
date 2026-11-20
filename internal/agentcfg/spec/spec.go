// Package spec loads the desired state: the MCP servers every machine must
// have, the only hooks and plugins allowed to exist, and the machines that
// may decrypt the shared secrets.
//
// The top level of the file is the base layer. Roles and the entry for the
// current host add to it, in that order, so a machine keeps its own servers
// and hooks while sharing everything in the base.
package spec

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"filippo.io/age"
	"github.com/BurntSushi/toml"

	"github.com/torturado/agentcfg/internal/model"
)

type Server struct {
	URL     string   `toml:"url"`
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	// Env and Headers hold literal values or ${VAR} references. A credential
	// must be a reference: the spec is synced to every machine.
	Env     map[string]string `toml:"env"`
	Headers map[string]string `toml:"headers"`
	// Agents that must have the server. Empty means every supported agent.
	Agents []model.Agent `toml:"agents"`
}

// Layer is one set of declarations: the base, a role or a host.
type Layer struct {
	MCPServers map[string]Server `toml:"mcp_servers"`
	// Hooks lists, per agent, the hook commands allowed to exist. Any other
	// hook is reported; an agent without an entry allows none.
	Hooks map[model.Agent][]string `toml:"hooks"`
	// Plugins lists the Claude Code plugins (name@marketplace) allowed to be
	// enabled machine-wide.
	Plugins map[model.Agent][]string `toml:"plugins"`
}

type Host struct {
	Layer
	Roles []string `toml:"roles"`
	// AgeRecipient is the machine's age public key; secrets.age is encrypted
	// to every host that has one.
	AgeRecipient string `toml:"age_recipient"`
}

// File is the spec as written. Hosts are keyed by hostname or tailnet name.
type File struct {
	Layer
	Roles map[string]Layer `toml:"roles"`
	Hosts map[string]Host  `toml:"hosts"`
}

// Spec is the desired state of one machine after merging its layers.
type Spec struct {
	// Host is the [hosts] entry that matched, or empty when none did.
	Host       string
	MCPServers map[string]Server
	Hooks      map[model.Agent][]string
	Plugins    map[model.Agent][]string
}

func Load(path string) (File, error) {
	var f File
	meta, err := toml.DecodeFile(path, &f)
	if err != nil {
		return f, err
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return f, fmt.Errorf("%s: unknown keys %v", path, undecoded)
	}
	if err := f.Layer.validate("", path); err != nil {
		return f, err
	}
	for name, role := range f.Roles {
		if err := role.validate("roles."+name+".", path); err != nil {
			return f, err
		}
	}
	for name, host := range f.Hosts {
		if err := host.Layer.validate("hosts."+name+".", path); err != nil {
			return f, err
		}
		for _, role := range host.Roles {
			if _, ok := f.Roles[role]; !ok {
				return f, fmt.Errorf("%s: hosts.%s: unknown role %q", path, name, role)
			}
		}
		if host.AgeRecipient != "" {
			if _, err := age.ParseX25519Recipient(host.AgeRecipient); err != nil {
				return f, fmt.Errorf("%s: hosts.%s.age_recipient: %w", path, name, err)
			}
		}
	}
	return f, nil
}

// Resolve merges the base, the roles of the matching host and the host itself.
// names are the machine's hostname and tailnet names; at most one [hosts]
// entry may match them.
func (f File) Resolve(names []string) (Spec, error) {
	var matched []string
	for key := range f.Hosts {
		if slices.ContainsFunc(names, func(name string) bool { return strings.EqualFold(name, key) }) {
			matched = append(matched, key)
		}
	}
	slices.Sort(matched)
	if len(matched) > 1 {
		return Spec{}, fmt.Errorf("hosts %v all match this machine (%v)", matched, names)
	}

	s := Spec{MCPServers: map[string]Server{}, Hooks: map[model.Agent][]string{}, Plugins: map[model.Agent][]string{}}
	s.add(f.Layer)
	if len(matched) == 1 {
		s.Host = matched[0]
		host := f.Hosts[s.Host]
		for _, role := range host.Roles {
			s.add(f.Roles[role])
		}
		s.add(host.Layer)
	}
	return s, nil
}

// Recipients returns the age public keys of every host, sorted.
func (f File) Recipients() []string {
	var recipients []string
	for _, host := range f.Hosts {
		if host.AgeRecipient != "" && !slices.Contains(recipients, host.AgeRecipient) {
			recipients = append(recipients, host.AgeRecipient)
		}
	}
	slices.Sort(recipients)
	return recipients
}

// add merges a layer: a server replaces one of the same name, hooks and
// plugins accumulate.
func (s *Spec) add(layer Layer) {
	maps.Copy(s.MCPServers, layer.MCPServers)
	for agent, commands := range layer.Hooks {
		s.Hooks[agent] = append(s.Hooks[agent], commands...)
	}
	for agent, plugins := range layer.Plugins {
		s.Plugins[agent] = append(s.Plugins[agent], plugins...)
	}
}

func (s Server) AppliesTo(agent model.Agent) bool {
	return len(s.Agents) == 0 || slices.Contains(s.Agents, agent)
}

func (layer Layer) validate(prefix, path string) error {
	for name, server := range layer.MCPServers {
		where := fmt.Sprintf("%s: %smcp_servers.%s", path, prefix, name)
		if (server.URL == "") == (server.Command == "") {
			return fmt.Errorf("%s needs either url or command", where)
		}
		if server.URL != "" && (len(server.Args) > 0 || len(server.Env) > 0) {
			return fmt.Errorf("%s: args and env only apply to a command", where)
		}
		if server.Command != "" && len(server.Headers) > 0 {
			return fmt.Errorf("%s: headers only apply to a url", where)
		}
		for _, agent := range server.Agents {
			if !slices.Contains(model.Agents, agent) {
				return fmt.Errorf("%s: unknown agent %q", where, agent)
			}
		}
		for key, value := range server.Headers {
			if err := checkValue(where+".headers."+key, key, value); err != nil {
				return err
			}
		}
		for key, value := range server.Env {
			if err := checkValue(where+".env."+key, key, value); err != nil {
				return err
			}
		}
		if server.AppliesTo(model.Codex) {
			if _, err := CodexServer(server); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
		}
	}
	for agent := range layer.Hooks {
		if !slices.Contains(model.Agents, agent) {
			return fmt.Errorf("%s: %shooks: unknown agent %q", path, prefix, agent)
		}
	}
	for agent := range layer.Plugins {
		if agent != model.Claude {
			return fmt.Errorf("%s: %splugins: only claude plugins are supported, not %q", path, prefix, agent)
		}
	}
	return nil
}

// checkValue rejects a credential written into the spec itself.
func checkValue(where, name, value string) error {
	if model.SecretName.MatchString(name) && value != "" && !model.EnvRef.MatchString(value) {
		return fmt.Errorf("%s: credential written in the spec; reference a variable such as ${NAME}", where)
	}
	return nil
}

// CodexMCPServer is a spec server in the shape of a Codex config.toml
// mcp_servers entry.
type CodexMCPServer struct {
	URL               string
	HTTPHeaders       map[string]string
	EnvHTTPHeaders    map[string]string
	BearerTokenEnvVar string
	Command           string
	Args              []string
	Env               map[string]string
	EnvVars           []string
}

// CodexServer translates references into what Codex supports: a header that
// is exactly ${VAR}, an Authorization header "Bearer ${VAR}", and a variable
// passed through under its own name.
func CodexServer(s Server) (CodexMCPServer, error) {
	c := CodexMCPServer{URL: s.URL, Command: s.Command, Args: s.Args}
	for _, header := range slices.Sorted(maps.Keys(s.Headers)) {
		value := s.Headers[header]
		refs := model.EnvRef.FindAllStringSubmatch(value, -1)
		switch {
		case len(refs) == 0:
			c.HTTPHeaders = setKey(c.HTTPHeaders, header, value)
		case len(refs) == 1 && refs[0][0] == value:
			c.EnvHTTPHeaders = setKey(c.EnvHTTPHeaders, header, refs[0][1])
		case len(refs) == 1 && strings.EqualFold(header, "Authorization") && value == "Bearer "+refs[0][0]:
			c.BearerTokenEnvVar = refs[0][1]
		default:
			return c, fmt.Errorf("header %s: Codex only supports a value that is exactly ${VAR}, or \"Bearer ${VAR}\" for Authorization", header)
		}
	}
	for _, variable := range slices.Sorted(maps.Keys(s.Env)) {
		value := s.Env[variable]
		refs := model.EnvRef.FindAllStringSubmatch(value, -1)
		switch {
		case len(refs) == 0:
			c.Env = setKey(c.Env, variable, value)
		case len(refs) == 1 && value == "${"+variable+"}":
			c.EnvVars = append(c.EnvVars, variable)
		default:
			return c, fmt.Errorf("env %s: Codex can only pass a variable through under its own name, ${%s}", variable, variable)
		}
	}
	return c, nil
}

func setKey(m map[string]string, key, value string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[key] = value
	return m
}
