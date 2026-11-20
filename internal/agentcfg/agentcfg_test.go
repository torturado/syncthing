package agentcfg_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/torturado/agentcfg"
)

var secretVariables = []string{"SEARCH_API_KEY", "DEPLOY_TOKEN", "EVENTS_TOKEN"}

// copyHome copies a fixture home into a temporary directory, replacing @HOME@
// with its path, so tests can write to it and absolute paths resolve.
func copyHome(t *testing.T, fixture string) string {
	t.Helper()
	home := t.TempDir()
	src := filepath.Join("testdata", fixture)
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		dst := filepath.Join(home, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, []byte(strings.ReplaceAll(string(data), "@HOME@", home)), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return home
}

func options(t *testing.T, home, specPath, host string) agentcfg.Options {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	return agentcfg.Options{SpecPath: specPath, Home: home, HostNames: []string{host}, BackupDir: filepath.Join(t.TempDir(), "backups")}
}

// findings runs Check and returns each finding as "agent severity rule location".
func findings(t *testing.T, opts agentcfg.Options) []string {
	t.Helper()
	report, err := agentcfg.Check(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, f := range report.Findings {
		got = append(got, string(f.Agent)+" "+string(f.Severity)+" "+f.Rule+" "+f.Location)
	}
	slices.Sort(got)
	return got
}

func setVariables(t *testing.T, set bool) {
	t.Helper()
	for _, name := range secretVariables {
		t.Setenv(name, "value")
		if !set {
			os.Unsetenv(name)
		}
	}
}

func assertFindings(t *testing.T, got, want []string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("findings differ\n got: %q\nwant: %q", got, want)
	}
}

var baseSpec = filepath.Join("testdata", "spec.toml")

func TestCleanMachineHasNoFindings(t *testing.T) {
	setVariables(t, true)
	assertFindings(t, findings(t, options(t, copyHome(t, "clean"), baseSpec, "casa")), nil)
}

func TestReferencedVariablesMissingFromEnvironmentAreWarnings(t *testing.T) {
	setVariables(t, false)
	assertFindings(t, findings(t, options(t, copyHome(t, "clean"), baseSpec, "casa")), []string{
		"claude warning unset-variable mcpServers.search.headers.X-API-Key",
		"claude warning unset-variable mcpServers.deploy.headers.Authorization",
		"codex warning unset-variable mcp_servers.search.env_http_headers.X-API-Key",
		"codex warning unset-variable mcp_servers.deploy.bearer_token_env_var",
	})
}

func TestReferenceInSettingsEnvIsReportedAsUnexpanded(t *testing.T) {
	setVariables(t, true)
	home := copyHome(t, "clean")
	settings := `{ "env": { "ANTHROPIC_AUTH_TOKEN": "${PROXY_TOKEN}" }, "enabledMcpjsonServers": ["search", "deploy"] }`
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	assertFindings(t, findings(t, options(t, home, baseSpec, "casa")), []string{
		"claude error unexpanded-variable env.ANTHROPIC_AUTH_TOKEN",
	})
}

var driftedFindings = []string{
	"claude error literal-secret env.ANTHROPIC_AUTH_TOKEN",
	"claude error literal-secret mcpServers.search.headers.X-API-Key",
	"claude error literal-secret projects./home/user.mcpServers.deploy.headers.Authorization",
	"claude error server-mismatch deploy",
	"claude error server-mismatch search",
	"claude error undeclared-hook SessionStart",
	"claude error undeclared-server research",
	"codex error literal-secret mcp_servers.search.http_headers.X-API-Key",
	"codex error server-mismatch search",
	"codex error server-mismatch search",
	"codex error undeclared-hook PreToolUse",
	"codex error undeclared-hook SessionStart",
	"codex error undeclared-hook Stop",
	"codex warning unset-variable mcp_servers.deploy.bearer_token_env_var",
}

func TestDriftedMachineReportsEveryDeviation(t *testing.T) {
	setVariables(t, false)
	assertFindings(t, findings(t, options(t, copyHome(t, "drifted"), baseSpec, "casa")), driftedFindings)
}

func TestMissingServerIsReported(t *testing.T) {
	setVariables(t, true)
	assertFindings(t, findings(t, options(t, t.TempDir(), baseSpec, "casa")), []string{
		"claude error missing-server mcp_servers.deploy",
		"claude error missing-server mcp_servers.search",
		"codex error missing-server mcp_servers.deploy",
		"codex error missing-server mcp_servers.search",
	})
}

func TestEnabledPluginHooksAndServersAreReported(t *testing.T) {
	setVariables(t, true)
	assertFindings(t, findings(t, options(t, copyHome(t, "plugin"), baseSpec, "casa")), []string{
		"claude error undeclared-hook SessionStart",
		"claude error undeclared-plugin enabledPlugins.notify@example",
		"claude error undeclared-plugin enabledPlugins.skills-only@example",
		"claude error undeclared-server plugin:notify:events",
		"codex error missing-server mcp_servers.deploy",
		"codex error missing-server mcp_servers.search",
	})
}

func TestDeclaredPluginAndItsHookPass(t *testing.T) {
	setVariables(t, true)
	specPath := filepath.Join(t.TempDir(), "spec.toml")
	spec, _ := os.ReadFile(baseSpec)
	spec = append(spec, []byte(`claude = ["node ${CLAUDE_PLUGIN_ROOT}/hooks/post.js start"]

[plugins]
claude = ["notify@example", "skills-only@example"]

[mcp_servers."plugin:notify:events"]
url = "https://events.example.com/mcp"
headers = { Authorization = "Bearer ${EVENTS_TOKEN}" }
agents = ["claude"]
`)...)
	if err := os.WriteFile(specPath, spec, 0o600); err != nil {
		t.Fatal(err)
	}
	assertFindings(t, findings(t, options(t, copyHome(t, "plugin"), specPath, "casa")), []string{
		"codex error missing-server mcp_servers.deploy",
		"codex error missing-server mcp_servers.search",
	})
}

var fleetSpec = filepath.Join("testdata", "fleet", "spec.toml")

func TestHostLayerKeepsMachineSettingsWhileSharingBase(t *testing.T) {
	setVariables(t, true)
	home := copyHome(t, filepath.Join("fleet", "aimery"))
	opts := options(t, home, fleetSpec, "aimery")
	settingsBefore, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))

	assertFindings(t, findings(t, opts), nil)

	report, err := agentcfg.Apply(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Files) != 0 {
		t.Errorf("apply changed a machine that matches its layers: %+v", report.Files)
	}
	settingsAfter, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if string(settingsBefore) != string(settingsAfter) {
		t.Error("apply changed aimery's settings.json")
	}

	// When the machine's names match no entry, for instance because tailscale
	// did not answer, the base alone would remove its own server and hook.
	unknown := options(t, home, fleetSpec, "other")
	if _, err := agentcfg.Check(context.Background(), unknown); err == nil {
		t.Error("check accepted a machine that matches no [hosts] entry")
	}
	if _, err := agentcfg.Apply(context.Background(), unknown); err == nil {
		t.Error("apply accepted a machine that matches no [hosts] entry")
	}
	settingsAfter, _ = os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if string(settingsBefore) != string(settingsAfter) {
		t.Error("apply changed the settings of a machine that matches no [hosts] entry")
	}
}

func TestApplyCorrectsDriftAndKeepsAgentState(t *testing.T) {
	setVariables(t, false)
	home := copyHome(t, "drifted")
	opts := options(t, home, baseSpec, "casa")

	projectsBefore := claudeProjects(t, home)
	codexBefore := codexConfig(t, home)

	report, err := agentcfg.Apply(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Written || len(report.Files) != 4 {
		t.Fatalf("expected 4 files written, got %+v", report)
	}
	for _, file := range report.Files {
		if file.Backup == "" {
			t.Errorf("%s: no backup", file.Path)
		}
	}

	if got := claudeProjects(t, home); !jsonEqual(got, projectsBefore) {
		t.Errorf("projects in ~/.claude.json changed\n got: %s\nwant: %s", got, projectsBefore)
	}
	codexAfter := codexConfig(t, home)
	stateBefore := codexBefore["hooks"].(map[string]any)["state"]
	stateAfter := codexAfter["hooks"].(map[string]any)["state"]
	if !jsonEqual(mustJSON(stateAfter), mustJSON(stateBefore)) {
		t.Errorf("hooks.state changed\n got: %v\nwant: %v", stateAfter, stateBefore)
	}
	if !jsonEqual(mustJSON(codexAfter["projects"]), mustJSON(codexBefore["projects"])) {
		t.Error("Codex projects changed")
	}

	// What remains is outside the keys apply owns: a credential in the
	// settings env and the servers of a local project scope.
	assertFindings(t, findings(t, opts), []string{
		"claude error literal-secret env.ANTHROPIC_AUTH_TOKEN",
		"claude error literal-secret projects./home/user.mcpServers.deploy.headers.Authorization",
		"claude error server-mismatch deploy",
		"claude error undeclared-server research",
		"claude warning shadowed-server deploy",
		"claude warning unset-variable mcpServers.deploy.headers.Authorization",
		"claude warning unset-variable mcpServers.search.headers.X-API-Key",
		"codex warning unset-variable mcp_servers.deploy.bearer_token_env_var",
		"codex warning unset-variable mcp_servers.search.env_http_headers.X-API-Key",
	})

	again, err := agentcfg.Apply(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Files) != 0 {
		t.Errorf("second apply changed %+v", again.Files)
	}
}

func TestApplyWithoutWriteChangesNothing(t *testing.T) {
	home := copyHome(t, "drifted")
	before := snapshot(t, home)
	report, err := agentcfg.Plan(context.Background(), options(t, home, baseSpec, "casa"))
	if err != nil {
		t.Fatal(err)
	}
	if report.Written || len(report.Files) == 0 {
		t.Fatalf("plan should list changes without writing: %+v", report)
	}
	if after := snapshot(t, home); !maps.Equal(before, after) {
		t.Error("plan wrote files")
	}
}

func TestApplyWritesThroughSymlinkAndKeepsComments(t *testing.T) {
	home := copyHome(t, "drifted")
	real := filepath.Join(t.TempDir(), "codex-config.toml")
	link := filepath.Join(home, ".codex", "config.toml")
	data, _ := os.ReadFile(link)
	if err := os.WriteFile(real, append([]byte("# managed by hand\nmodel = \"example-model\"\n\n"), data...), 0o640); err != nil {
		t.Fatal(err)
	}
	os.Remove(link)
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if _, err := agentcfg.Apply(context.Background(), options(t, home, baseSpec, "casa")); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(link); err != nil || target != real {
		t.Fatalf("symlink not kept: %q %v", target, err)
	}
	after, _ := os.ReadFile(real)
	if !strings.HasPrefix(string(after), "# managed by hand\nmodel = \"example-model\"\n") {
		t.Errorf("comment or unrelated key lost:\n%s", after)
	}
	if info, _ := os.Stat(real); info.Mode().Perm() != 0o640 {
		t.Errorf("mode changed to %v", info.Mode().Perm())
	}
	if strings.Contains(string(after), "fake-search-key") || strings.Contains(string(after), "notify-send") {
		t.Errorf("literal header or undeclared hook left:\n%s", after)
	}
}

// TestApplyCorrectsHookReinstall reproduces a tool reinstalling its hooks into
// an otherwise clean home.
func TestApplyCorrectsHookReinstall(t *testing.T) {
	setVariables(t, true)
	home := copyHome(t, "clean")
	opts := options(t, home, baseSpec, "casa")
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.WriteFile(settings, []byte(`{
  "env": { "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1" },
  "enabledMcpjsonServers": ["search", "deploy"],
  "hooks": {
    "SessionStart": [
      { "matcher": "^(startup|resume)$", "hooks": [ { "type": "command", "command": "bash '/home/user/.claude/hooks/herdr-agent-state.sh' session", "timeout": 10 } ] }
    ]
  }
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(home, ".codex", "hooks.json")
	drifted, _ := os.ReadFile(filepath.Join("testdata", "drifted", ".codex", "hooks.json"))
	if err := os.WriteFile(hooks, drifted, 0o644); err != nil {
		t.Fatal(err)
	}
	assertFindings(t, findings(t, opts), []string{
		"claude error undeclared-hook SessionStart",
		"codex error undeclared-hook PreToolUse",
		"codex error undeclared-hook SessionStart",
	})

	if _, err := agentcfg.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	assertFindings(t, findings(t, opts), nil)

	var s map[string]any
	data, _ := os.ReadFile(settings)
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s["env"] == nil || s["enabledMcpjsonServers"] == nil {
		t.Errorf("other settings lost: %s", data)
	}
}

func claudeProjects(t *testing.T, home string) json.RawMessage {
	t.Helper()
	var state map[string]json.RawMessage
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state["projects"]
}

func codexConfig(t *testing.T, home string) map[string]any {
	t.Helper()
	var doc map[string]any
	if _, err := toml.DecodeFile(filepath.Join(home, ".codex", "config.toml"), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["projects"] == nil {
		doc["projects"] = map[string]any{}
	}
	return doc
}

func mustJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	_ = json.Unmarshal(a, &x)
	_ = json.Unmarshal(b, &y)
	return string(mustJSON(x)) == string(mustJSON(y))
}

func snapshot(t *testing.T, home string) map[string]string {
	t.Helper()
	files := map[string]string{}
	_ = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := os.ReadFile(path)
			files[path] = string(data)
		}
		return nil
	})
	return files
}

func TestSecretsLiveNextToTheSpecALinkPointsTo(t *testing.T) {
	synced := t.TempDir()
	spec := filepath.Join(synced, "agentcfg.toml")
	if err := os.WriteFile(spec, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "spec.toml")
	if err := os.Symlink(spec, link); err != nil {
		t.Fatal(err)
	}
	if got, want := agentcfg.SecretsPath(link), filepath.Join(synced, "secrets.age"); got != want {
		t.Errorf("SecretsPath = %s, want %s", got, want)
	}
}

// TestApplyCorrectsServersWhereTheyAreAndKeepsTheirOptions changes a declared
// URL while the server lives in ~/.mcp.json, as on a machine where that file
// is linked to a synced one, and while its Codex table carries options the
// spec does not describe.
func TestApplyCorrectsServersWhereTheyAreAndKeepsTheirOptions(t *testing.T) {
	setVariables(t, true)
	home := copyHome(t, "clean")
	specPath := filepath.Join(t.TempDir(), "spec.toml")
	spec, _ := os.ReadFile(baseSpec)
	spec = []byte(strings.Replace(string(spec), "https://search.example.com/mcp", "https://search.example.org/mcp", 1))
	if err := os.WriteFile(specPath, spec, 0o600); err != nil {
		t.Fatal(err)
	}
	mcpJSON := `{ "mcpServers": {
  "search": { "type": "http", "url": "https://search.example.com/mcp", "headers": { "X-API-Key": "${SEARCH_API_KEY}" }, "timeout": 30 },
  "deploy": { "type": "http", "url": "https://deploy.example.ts.net/mcp", "headers": { "Authorization": "Bearer ${DEPLOY_TOKEN}" } },
  "notes": { "type": "http", "url": "https://notes.example.com/mcp" },
  "unapproved": { "type": "http", "url": "https://unapproved.example.com/mcp" }
} }`
	// Approving all servers does not override an explicit rejection.
	settings := `{ "enableAllProjectMcpServers": true, "disabledMcpjsonServers": ["unapproved"] }`
	codex := strings.Replace(codexText(t, home), "[mcp_servers.search]\n", "[mcp_servers.search]\n# kept by hand\nstartup_timeout_sec = 30\n", 1)
	codex = strings.Replace(codex, `env_http_headers = { "X-API-Key" = "SEARCH_API_KEY" }`, `env_http_headers = { "X-API-Key" = "SEARCH_API_KEY" } # unchanged, so kept`, 1)
	for path, content := range map[string]string{".mcp.json": mcpJSON, ".claude/settings.json": settings, ".codex/config.toml": codex} {
		if err := os.WriteFile(filepath.Join(home, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	opts := options(t, home, specPath, "casa")

	if _, err := agentcfg.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	assertFindings(t, findings(t, opts), nil)

	var file struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	data, _ := os.ReadFile(filepath.Join(home, ".mcp.json"))
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if got := file.MCPServers["search"]; got["url"] != "https://search.example.org/mcp" || got["timeout"] != float64(30) {
		t.Errorf("search in ~/.mcp.json not corrected in place: %v", got)
	}
	if _, found := file.MCPServers["notes"]; found {
		t.Error("undeclared approved server left in ~/.mcp.json")
	}
	if _, found := file.MCPServers["unapproved"]; !found {
		t.Error("rejected server, which never loads, removed from ~/.mcp.json")
	}
	if state, _ := os.ReadFile(filepath.Join(home, ".claude.json")); strings.Contains(string(state), "search") {
		t.Errorf("search duplicated in ~/.claude.json:\n%s", state)
	}
	if after := codexText(t, home); !strings.Contains(after, "# kept by hand\n") || !strings.Contains(after, "startup_timeout_sec = 30\n") || !strings.Contains(after, "# unchanged, so kept") {
		t.Errorf("Codex server option or comment lost:\n%s", after)
	}
}

func codexText(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestReportsDoNotCarryCredentials puts a token in a hook command and in a URL
// query, which reports name, and checks that neither reaches a report.
func TestReportsDoNotCarryCredentials(t *testing.T) {
	setVariables(t, true)
	home := copyHome(t, "clean")
	settings := `{
  "enabledMcpjsonServers": ["search", "deploy"],
  "hooks": { "Stop": [{ "hooks": [{ "type": "command", "command": "curl -H 'Authorization: Bearer hook-secret-1234' https://hooks.example.com/stop?token=hook-secret-5678" },
    { "type": "command", "command": "notify --token \"hook-secret-quoted\" && echo token api_key=hook-secret-after-prose" },
    { "type": "command", "command": "sh -c 'curl --oauth2-bearer hook-secret-nested -d {\"api_key\":\"hook-secret-json\"} https://u:hook-secret-userinfo@h.example.com'" },
    { "type": "command", "command": "notify --token '$hook-secret-dollar' --oauth2-bearer -hook-secret-dash -d '{\"model\":\"x\",\"api_key\":\"hook-secret-second\",\"u\":\"https://u:hook-secret-json-url@h.example.com/\"}' --password \\\nhook-secret-continued" },
    { "type": "command", "command": "x --token \\$escaped_value --password Basic -d '{\"api_key\":\"a\\\"hook-secret-escaped-quote\"}' 'http://[::1]:80/x?a=1,hook-secret-comma&t=hook-secret-ipv6' --password=\"two hook-secret-words\"" },
    { "type": "command", "command": "curl --header=\"Authorization: Bearer hook-secret-header\" -d '{\"kind\":\"x\",\"api_key\":\"abc\\\" hook-secret-after-space\"}' https://a.example/,https://u:hook-secret-list@b.example/" }] }] }
}`
	mcpJSON := strings.Replace(readFile(t, filepath.Join(home, ".mcp.json")), "https://search.example.com/mcp", "https://search.example.com/mcp?key=url-secret-1234", 1)
	for path, content := range map[string]string{".claude/settings.json": settings, ".mcp.json": mcpJSON} {
		if err := os.WriteFile(filepath.Join(home, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	opts := options(t, home, baseSpec, "casa")
	report, err := agentcfg.Check(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := agentcfg.Plan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) == 0 || len(plan.Files) == 0 {
		t.Fatalf("expected drift to report: %+v %+v", report, plan)
	}
	for _, value := range []any{report, plan} {
		text := string(mustJSON(value))
		if strings.Contains(text, "secret-") || strings.Contains(text, "Basic") || strings.Contains(text, "escaped_value") {
			t.Errorf("credential in report:\n%s", text)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
