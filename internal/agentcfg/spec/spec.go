// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Package spec loads the desired state: the MCP servers every machine must
// have, the only hooks and plugins allowed to exist, the gateway Antigravity
// sends its model requests to, the machines that may decrypt the shared
// secrets, and what stays out of the synced folders.
//
// The top level of the file is the base layer. Roles and the entry for the
// current host add to it, in that order, so a machine keeps its own servers
// and hooks while sharing everything in the base.
package spec

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"filippo.io/age"
	"github.com/BurntSushi/toml"

	"github.com/syncthing/syncthing/internal/agentcfg/model"
	"github.com/syncthing/syncthing/internal/agentcfg/secrets"
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
	// Plugins lists the plugins allowed: for Claude Code those enabled
	// machine-wide (name@marketplace), for Antigravity those installed in
	// ~/.gemini/config/plugins (the directory name).
	Plugins map[model.Agent][]string `toml:"plugins"`
}

type Host struct {
	Layer

	Roles []string `toml:"roles"`
	// AgeRecipient is the machine's age public key; secrets.age is encrypted
	// to every host that has one.
	AgeRecipient string `toml:"age_recipient"`
}

// Sync is how the folders of agent configuration are synced. It applies to
// the whole fleet, so it has no role or host layers.
type Sync struct {
	// Exclude lists, per folder in model.SyncFolders, Syncthing ignore
	// patterns for files that stay on each machine.
	Exclude map[string][]string `toml:"exclude"`
}

// Antigravity is what only Antigravity is configured with. Like Sync, it
// applies to the whole fleet.
type Antigravity struct {
	Gateway *Gateway `toml:"gateway"`
}

// Gateway sends Antigravity's model requests to an enterprise gateway
// instead of Google. agy and its ACP server read it from AGY_LLM_GATEWAY_*
// environment variables, which apply writes on each machine.
type Gateway struct {
	URL string `toml:"url"`
	// APIKey is a ${VAR} reference: the spec is synced.
	APIKey string `toml:"api_key"`
	// WireProtocol is "genai", agy's default, or "openai". The ACP server,
	// which editors and T3 Code run, supports only "genai".
	WireProtocol string `toml:"wire_protocol"`
	// Models are the models agy offers, the first one by default. Empty
	// lets agy list them from the gateway.
	Models []string `toml:"models"`
}

// File is the spec as written. Hosts are keyed by hostname or tailnet name.
type File struct {
	Layer

	Roles       map[string]Layer `toml:"roles"`
	Hosts       map[string]Host  `toml:"hosts"`
	Sync        Sync             `toml:"sync"`
	Antigravity Antigravity      `toml:"antigravity"`
}

// Spec is the desired state of one machine after merging its layers.
type Spec struct {
	// Host is the [hosts] entry that matched, or empty when none did.
	Host       string
	MCPServers map[string]Server
	Hooks      map[model.Agent][]string
	Plugins    map[model.Agent][]string
	// Gateway is nil when Antigravity talks to Google directly.
	Gateway *Gateway
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
	if err := f.Layer.validate("", path, nil); err != nil {
		return f, err
	}
	if err := f.Sync.validate(path); err != nil {
		return f, err
	}
	if gateway := f.Antigravity.Gateway; gateway != nil {
		if err := gateway.validate(); err != nil {
			return f, fmt.Errorf("%s: antigravity.gateway: %w", path, err)
		}
	}
	for name, role := range f.Roles {
		if err := role.validate("roles."+name+".", path, f.MCPServers); err != nil {
			return f, err
		}
	}
	for name, host := range f.Hosts {
		if err := host.Layer.validate("hosts."+name+".", path, f.MCPServers); err != nil {
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
// entry may match them. When the spec declares hosts, exactly one must match:
// the base alone would make apply remove what that machine adds.
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
	if len(f.Hosts) > 0 && len(matched) == 0 {
		return Spec{}, fmt.Errorf("no [hosts] entry matches this machine (%v); add one, or select it with --host", names)
	}

	s := Spec{MCPServers: map[string]Server{}, Hooks: map[model.Agent][]string{}, Plugins: map[model.Agent][]string{}, Gateway: f.Antigravity.Gateway}
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

// validate checks one layer. shared holds the top-level servers when the
// layer is a role or a host, and is nil for the top level itself.
func (layer Layer) validate(prefix, path string, shared map[string]Server) error {
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
		// Codex keeps its servers in ~/.codex/config.toml, which is synced,
		// so a server that differs between machines would be added by some
		// and removed by the rest, forever. That includes a role or host
		// server that replaces a top-level one Codex has.
		if base, replaces := shared[name]; replaces && base.AppliesTo(model.Codex) {
			return fmt.Errorf("%s replaces a top-level server that reaches Codex, whose config.toml is the same on every machine; give it another name", where)
		}
		if server.AppliesTo(model.Codex) {
			if prefix != "" {
				return fmt.Errorf("%s reaches Codex, whose config.toml is the same on every machine; declare it at the top level or leave codex out of its agents", where)
			}
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
		if agent != model.Claude && agent != model.Antigravity {
			return fmt.Errorf("%s: %splugins: only claude and antigravity plugins are supported, not %q", path, prefix, agent)
		}
	}
	return nil
}

// validate accepts exclusions only. A negated pattern would sync what the
// folder keeps local, such as credentials, and a directive would read
// patterns from elsewhere.
func (s Sync) validate(path string) error {
	for folder, patterns := range s.Exclude {
		where := fmt.Sprintf("%s: sync.exclude.%s", path, folder)
		if !slices.Contains(model.SyncFolders, folder) {
			return fmt.Errorf("%s: unknown folder; the synced folders are %s", where, strings.Join(model.SyncFolders, ", "))
		}
		for _, pattern := range patterns {
			bare := pattern
			for {
				rest, ok := strings.CutPrefix(bare, "(?i)")
				if !ok {
					rest, ok = strings.CutPrefix(bare, "(?d)")
				}
				if !ok {
					break
				}
				bare = rest
			}
			switch {
			case strings.TrimSpace(bare) == "" || strings.ContainsAny(pattern, "\r\n"):
				return fmt.Errorf("%s: %q is not a pattern", where, pattern)
			case strings.HasPrefix(bare, "!"):
				return fmt.Errorf("%s: %q includes files; only exclusions are allowed", where, pattern)
			case strings.HasPrefix(bare, "#") || strings.HasPrefix(bare, "//"):
				return fmt.Errorf("%s: %q is a directive or a comment, not a pattern", where, pattern)
			}
		}
	}
	return nil
}

// wholeRef is a value that is exactly one ${VAR} reference, without a default.
var wholeRef = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

func (g Gateway) validate() error {
	parsed, err := url.Parse(g.URL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return fmt.Errorf("url %q is not an http or https URL", g.URL)
	}
	if !wholeRef.MatchString(g.APIKey) {
		return errors.New("api_key must be a reference such as ${NAME}: the spec is synced")
	}
	if g.WireProtocol != "" && g.WireProtocol != "genai" && g.WireProtocol != "openai" {
		return fmt.Errorf("wire_protocol %q is neither genai nor openai", g.WireProtocol)
	}
	// The values end up in an environment file, which only takes values
	// that systemd and POSIX shells read alike.
	if err := secrets.ValidateValue("url", g.URL); err != nil {
		return err
	}
	for _, name := range g.Models {
		if err := secrets.ValidateValue("models", name); err != nil {
			return err
		}
		if strings.Contains(name, ",") {
			return fmt.Errorf("models: %q contains a comma, which separates the models agy reads", name)
		}
	}
	return nil
}

// Environment returns the AGY_LLM_GATEWAY_* variables for the gateway, with
// the API key that lookup returns for its reference. missing names the
// variable when lookup has no value for it; the variables cannot be written
// until it has.
func (g Gateway) Environment(lookup func(string) (string, bool)) (env map[string]string, missing []string) {
	variable := wholeRef.FindStringSubmatch(g.APIKey)[1]
	key, ok := lookup(variable)
	if !ok || key == "" {
		return nil, []string{variable}
	}
	env = map[string]string{"AGY_LLM_GATEWAY_URL": g.URL, "AGY_LLM_GATEWAY_API_KEY": key}
	if g.WireProtocol != "" {
		env["AGY_LLM_GATEWAY_WIRE_PROTOCOL"] = g.WireProtocol
	}
	if len(g.Models) > 0 {
		env["AGY_LLM_GATEWAY_MODELS"] = strings.Join(g.Models, ",")
	}
	return env, nil
}

// checkValue rejects a credential written into the spec itself.
func checkValue(where, name, value string) error {
	if model.SecretName.MatchString(name) && value != "" && !model.EnvRef.MatchString(value) {
		return fmt.Errorf("%s: credential written in the spec; reference a variable such as ${NAME}", where)
	}
	return nil
}

// reference is a ${VAR} or ${VAR:-default} reference, capturing the default.
var reference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// Expand replaces the references in a server with the values lookup
// returns, as Claude Code expands them: a default applies when the variable
// is unset or empty. It returns the variables without a value or default;
// while there are any, the server cannot be written.
func (s Server) Expand(lookup func(string) (string, bool)) (Server, []string) {
	var missing []string
	expand := func(value string) string {
		return reference.ReplaceAllStringFunc(value, func(ref string) string {
			match := reference.FindStringSubmatch(ref)
			if value, ok := lookup(match[1]); ok && value != "" {
				return value
			}
			if strings.Contains(ref, ":-") {
				return match[2]
			}
			if !slices.Contains(missing, match[1]) {
				missing = append(missing, match[1])
			}
			return ref
		})
	}
	out := s
	out.URL, out.Command = expand(s.URL), expand(s.Command)
	out.Args = nil
	for _, arg := range s.Args {
		out.Args = append(out.Args, expand(arg))
	}
	out.Env, out.Headers = expandValues(s.Env, expand), expandValues(s.Headers, expand)
	return out, missing
}

func expandValues(values map[string]string, expand func(string) string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = expand(value)
	}
	return out
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
		case len(refs) == 1 && value == "${"+refs[0][1]+"}":
			c.EnvHTTPHeaders = setKey(c.EnvHTTPHeaders, header, refs[0][1])
		case len(refs) == 1 && strings.EqualFold(header, "Authorization") && value == "Bearer ${"+refs[0][1]+"}":
			c.BearerTokenEnvVar = refs[0][1]
		default:
			return c, fmt.Errorf("header %s: Codex only supports a value that is exactly ${VAR}, or \"Bearer ${VAR}\" for Authorization, without a :- default", header)
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
