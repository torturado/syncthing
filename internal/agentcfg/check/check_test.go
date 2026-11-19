package check_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/torturado/agentcfg/internal/check"
	"github.com/torturado/agentcfg/internal/claude"
	"github.com/torturado/agentcfg/internal/codex"
	"github.com/torturado/agentcfg/internal/model"
	"github.com/torturado/agentcfg/internal/spec"
)

var secretVariables = []string{"PROXY_TOKEN", "SEARCH_API_KEY", "DEPLOY_TOKEN"}

// runCheck reads the fixture home like the command does and returns each
// finding as "agent severity rule location".
func runCheck(t *testing.T, home string) []string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")

	desired, err := spec.Load(filepath.Join("testdata", "spec.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var inv model.Inventory
	for _, read := range []func(string) (model.Inventory, error){claude.Inventory, codex.Inventory} {
		agentInv, err := read(home)
		if err != nil {
			t.Fatal(err)
		}
		inv.Merge(agentInv)
	}

	var got []string
	for _, f := range check.Run(desired, inv) {
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
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("findings differ\n got: %q\nwant: %q", got, want)
	}
}

func TestCleanMachineHasNoFindings(t *testing.T) {
	setVariables(t, true)
	assertFindings(t, runCheck(t, filepath.Join("testdata", "clean")), nil)
}

func TestReferencedVariablesMissingFromEnvironmentAreWarnings(t *testing.T) {
	setVariables(t, false)
	assertFindings(t, runCheck(t, filepath.Join("testdata", "clean")), []string{
		"claude warning unset-variable env.ANTHROPIC_AUTH_TOKEN",
		"claude warning unset-variable mcpServers.search.headers.X-API-Key",
		"claude warning unset-variable mcpServers.deploy.headers.Authorization",
		"codex warning unset-variable mcp_servers.search.env_http_headers.X-API-Key",
		"codex warning unset-variable mcp_servers.deploy.bearer_token_env_var",
	})
}

func TestDriftedMachineReportsEveryDeviation(t *testing.T) {
	setVariables(t, false)
	assertFindings(t, runCheck(t, filepath.Join("testdata", "drifted")), []string{
		"claude error literal-secret env.ANTHROPIC_AUTH_TOKEN",
		"claude error literal-secret mcpServers.search.headers.X-API-Key",
		"claude error literal-secret projects./home/user.mcpServers.deploy.headers.Authorization",
		"claude error undeclared-hook SessionStart",
		"claude error undeclared-server research",
		"codex error literal-secret mcp_servers.search.http_headers.X-API-Key",
		"codex error server-mismatch search",
		"codex error undeclared-hook PreToolUse",
		"codex error undeclared-hook SessionStart",
		"codex error undeclared-hook Stop",
		"codex warning unset-variable mcp_servers.deploy.bearer_token_env_var",
	})
}

func TestMissingServerIsReported(t *testing.T) {
	setVariables(t, true)
	home := t.TempDir()
	assertFindings(t, runCheck(t, home), []string{
		"claude error missing-server mcp_servers.deploy",
		"claude error missing-server mcp_servers.search",
		"codex error missing-server mcp_servers.deploy",
		"codex error missing-server mcp_servers.search",
	})
}
