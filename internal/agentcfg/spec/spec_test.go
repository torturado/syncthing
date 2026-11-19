package spec_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/torturado/agentcfg/internal/spec"
)

func load(t *testing.T, content string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := spec.Load(path)
	return err
}

func TestLoadRejectsInvalidSpecs(t *testing.T) {
	cases := map[string]string{
		"unknown key":           "[mcp_servers.search]\nurl = \"https://example.com\"\nheaders = {}\n",
		"server without target": "[mcp_servers.search]\nagents = [\"claude\"]\n",
		"unknown server agent":  "[mcp_servers.search]\nurl = \"https://example.com\"\nagents = [\"cursor\"]\n",
		"unknown hook agent":    "[hooks]\ncursor = []\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if err := load(t, content); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestLoadAcceptsValidSpec(t *testing.T) {
	err := load(t, "[mcp_servers.search]\nurl = \"https://example.com\"\n\n[mcp_servers.files]\ncommand = \"mcp-files\"\nagents = [\"claude\"]\n\n[hooks]\nclaude = [\"notify\"]\n")
	if err != nil {
		t.Fatal(err)
	}
}
