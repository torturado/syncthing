// Command agentcfg checks that the AI coding agents on this machine match a
// declared configuration.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/torturado/agentcfg/internal/check"
	"github.com/torturado/agentcfg/internal/claude"
	"github.com/torturado/agentcfg/internal/codex"
	"github.com/torturado/agentcfg/internal/model"
	"github.com/torturado/agentcfg/internal/spec"
)

// Exit codes: 0 matches the spec, 1 findings with severity error, 2 usage or read failure.
func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprintln(os.Stderr, "usage: agentcfg check [--spec path]")
		return 2
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	specPath := flags.String("spec", defaultSpecPath(home), "desired-state file")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}

	desired, err := spec.Load(*specPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "spec:", err)
		return 2
	}

	var inv model.Inventory
	for _, read := range []func(string) (model.Inventory, error){claude.Inventory, codex.Inventory} {
		agentInv, err := read(home)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		inv.Merge(agentInv)
	}

	findings := check.Run(desired, inv)
	errors := 0
	out := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, f := range findings {
		if f.Severity == check.Error {
			errors++
		}
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\n", f.Agent, f.Severity, f.Rule, shorten(f.Source, home), f.Location, f.Message)
	}
	out.Flush()
	fmt.Printf("\n%d errors, %d warnings (%d MCP servers, %d hooks read)\n",
		errors, len(findings)-errors, len(inv.MCPServers), len(inv.Hooks))

	if errors > 0 {
		return 1
	}
	return 0
}

func defaultSpecPath(home string) string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "agentcfg", "spec.toml")
}

func shorten(path, home string) string {
	if rest, ok := strings.CutPrefix(path, home); ok {
		return "~" + rest
	}
	return path
}
