// Command agentcfg checks and corrects the configuration of the AI coding
// agents on this machine against a declared spec, and distributes the secrets
// that configuration references.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/torturado/agentcfg"
	"github.com/torturado/agentcfg/internal/secrets"
	"github.com/torturado/agentcfg/internal/spec"
)

// Exit codes, documented in the README:
const (
	exitOK = 0
	// exitDrift: check found an error-severity finding, or apply without
	// --write has changes to make.
	exitDrift = 1
	// exitFailure: bad usage, or a spec or configuration file that cannot be
	// read or written.
	exitFailure = 2
)

const usage = `usage:
  agentcfg check   [--spec path] [--host name] [--json]
  agentcfg apply   [--spec path] [--host name] [--json] [--write]
  agentcfg secrets keygen [--identity path]
  agentcfg secrets set NAME [--spec path] [--identity path]   (value read from stdin)
  agentcfg secrets unset NAME [--spec path] [--identity path]
  agentcfg secrets rekey [--spec path] [--identity path]
  agentcfg secrets sync [--spec path] [--identity path] [--out path]`

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return exitFailure
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailure
	}
	var code int
	switch args[0] {
	case "check":
		code, err = runCheck(ctx, args[1:], home, stdout)
	case "apply":
		code, err = runApply(ctx, args[1:], home, stdout)
	case "secrets":
		code, err = runSecrets(args[1:], home, stdin, stdout)
	default:
		err = errors.New(usage)
	}
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, err)
		}
		return exitFailure
	}
	return code
}

type commonFlags struct {
	set      *flag.FlagSet
	spec     *string
	host     *string
	json     *bool
	identity *string
}

func newFlags(name, home string) commonFlags {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	return commonFlags{
		set:      set,
		spec:     set.String("spec", filepath.Join(configDir(home), "spec.toml"), "desired-state file"),
		host:     set.String("host", "", "use this [hosts] entry instead of matching the hostname and tailnet name"),
		json:     set.Bool("json", false, "print the report as JSON"),
		identity: set.String("identity", filepath.Join(configDir(home), "identity.txt"), "this machine's age identity"),
	}
}

func (f commonFlags) options() agentcfg.Options {
	opts := agentcfg.Options{SpecPath: *f.spec}
	if *f.host != "" {
		opts.HostNames = []string{*f.host}
	}
	return opts
}

func runCheck(ctx context.Context, args []string, home string, stdout io.Writer) (int, error) {
	f := newFlags("check", home)
	if err := f.set.Parse(args); err != nil {
		return 0, err
	}
	report, err := agentcfg.Check(ctx, f.options())
	if err != nil {
		return 0, err
	}
	if *f.json {
		if err := printJSON(stdout, report); err != nil {
			return 0, err
		}
	} else {
		out := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		for _, finding := range report.Findings {
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\n", finding.Agent, finding.Severity, finding.Rule, shorten(finding.Source, home), finding.Location, finding.Message)
		}
		out.Flush()
		host := report.Host
		if host == "" {
			host = "none"
		}
		fmt.Fprintf(stdout, "\n%d errors, %d warnings (host entry: %s; %d MCP servers, %d hooks, %d plugins read)\n",
			report.Errors, report.Warnings, host, report.MCPServers, report.Hooks, report.Plugins)
	}
	if !report.OK() {
		return exitDrift, nil
	}
	return exitOK, nil
}

func runApply(ctx context.Context, args []string, home string, stdout io.Writer) (int, error) {
	f := newFlags("apply", home)
	writeFiles := f.set.Bool("write", false, "write the changes; without it apply only lists them")
	if err := f.set.Parse(args); err != nil {
		return 0, err
	}
	var report agentcfg.ApplyReport
	var err error
	if *writeFiles {
		report, err = agentcfg.Apply(ctx, f.options())
	} else {
		report, err = agentcfg.Plan(ctx, f.options())
	}
	if err != nil {
		return 0, err
	}
	if *f.json {
		if err := printJSON(stdout, report); err != nil {
			return 0, err
		}
	} else {
		for _, file := range report.Files {
			fmt.Fprintln(stdout, shorten(file.Path, home))
			for _, change := range file.Changes {
				fmt.Fprintln(stdout, "  "+change)
			}
			if file.Backup != "" {
				fmt.Fprintln(stdout, "  backup: "+shorten(file.Backup, home))
			}
		}
		switch {
		case len(report.Files) == 0:
			fmt.Fprintln(stdout, "Nothing to change.")
		case report.Written:
			fmt.Fprintf(stdout, "\n%d files written.\n", len(report.Files))
		default:
			fmt.Fprintf(stdout, "\n%d files would change. Run with --write to apply.\n", len(report.Files))
		}
	}
	if len(report.Files) > 0 && !report.Written {
		return exitDrift, nil
	}
	return exitOK, nil
}

func runSecrets(args []string, home string, stdin io.Reader, stdout io.Writer) (int, error) {
	if len(args) == 0 {
		return 0, errors.New(usage)
	}
	command, args := args[0], args[1:]
	var name string
	if command == "set" || command == "unset" {
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			return 0, fmt.Errorf("secrets %s needs a variable name", command)
		}
		name, args = args[0], args[1:]
		if err := secrets.ValidateName(name); err != nil {
			return 0, err
		}
	}
	f := newFlags("secrets "+command, home)
	out := f.set.String("out", filepath.Join(xdgConfig(home), "environment.d", "agentcfg.conf"), "environment file written by sync")
	if err := f.set.Parse(args); err != nil {
		return 0, err
	}
	secretsPath := agentcfg.SecretsPath(*f.spec)

	switch command {
	case "keygen":
		recipient, err := secrets.Keygen(*f.identity)
		if err != nil {
			return 0, err
		}
		fmt.Fprintf(stdout, "Identity written to %s.\nAdd this machine's public key to its [hosts] entry in the spec:\n\nage_recipient = %q\n", shorten(*f.identity, home), recipient)
		return exitOK, nil

	case "sync":
		values, err := secrets.Load(secretsPath, *f.identity)
		if err != nil {
			return 0, err
		}
		changed, err := secrets.Sync(values, *out)
		if err != nil {
			return 0, err
		}
		names := slices.Sorted(maps.Keys(values))
		state := "unchanged"
		if changed {
			state = "written"
		}
		fmt.Fprintf(stdout, "%s %s: %s\n", shorten(*out, home), state, strings.Join(names, ", "))
		return exitOK, nil

	case "set", "unset", "rekey":
		file, err := spec.Load(*f.spec)
		if err != nil {
			return 0, fmt.Errorf("spec: %w", err)
		}
		values, err := secrets.Load(secretsPath, *f.identity)
		if err != nil {
			return 0, err
		}
		switch command {
		case "set":
			value, err := readValue(stdin)
			if err != nil {
				return 0, err
			}
			if err := secrets.ValidateValue(name, value); err != nil {
				return 0, err
			}
			values[name] = value
		case "unset":
			if _, ok := values[name]; !ok {
				return 0, fmt.Errorf("%s is not in %s", name, shorten(secretsPath, home))
			}
			delete(values, name)
		}
		recipients := file.Recipients()
		if err := secrets.Save(secretsPath, values, recipients); err != nil {
			return 0, err
		}
		fmt.Fprintf(stdout, "%s encrypted to %d machines. Run agentcfg secrets sync on each one.\n", shorten(secretsPath, home), len(recipients))
		return exitOK, nil
	}
	return 0, errors.New(usage)
}

// readValue reads one line, so a value can be piped or typed without it
// appearing in the shell history.
func readValue(stdin io.Reader) (string, error) {
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func printJSON(stdout io.Writer, value any) error {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func xdgConfig(home string) string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return base
	}
	return filepath.Join(home, ".config")
}

func configDir(home string) string {
	return filepath.Join(xdgConfig(home), "agentcfg")
}

func shorten(path, home string) string {
	if rest, ok := strings.CutPrefix(path, home); ok {
		return "~" + rest
	}
	return path
}
