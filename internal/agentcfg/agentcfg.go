// Package agentcfg checks and corrects the configuration of the AI coding
// agents on a machine against a declared spec, so every machine in a fleet
// runs the same MCP servers, no hook or plugin appears unnoticed, and no
// credential sits in a synced file.
//
// Check only reads. Apply writes, and only the keys agentcfg owns.
package agentcfg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/torturado/agentcfg/internal/check"
	"github.com/torturado/agentcfg/internal/claude"
	"github.com/torturado/agentcfg/internal/codex"
	"github.com/torturado/agentcfg/internal/files"
	"github.com/torturado/agentcfg/internal/model"
	"github.com/torturado/agentcfg/internal/spec"
)

// SchemaVersion is the version of the JSON form of Report and ApplyReport. It
// changes only when a field is removed or changes meaning.
const SchemaVersion = 1

type (
	Agent    = model.Agent
	Severity = check.Severity
	Finding  = check.Finding
)

const (
	Error   = check.Error
	Warning = check.Warning
)

// backupsKept is how many apply runs keep their backups.
const backupsKept = 20

type Options struct {
	// SpecPath is the desired-state file. Required.
	SpecPath string
	// Home is the home directory whose configuration is read. Default: the
	// current user's.
	Home string
	// HostNames select the [hosts] entry of the spec. Default: DefaultHostNames.
	HostNames []string
	// BackupDir receives a directory per apply run with the files it replaced.
	// Default: $XDG_STATE_HOME/agentcfg/backups.
	BackupDir string
}

// Report is the result of Check.
type Report struct {
	SchemaVersion int       `json:"schema_version"`
	CheckedAt     time.Time `json:"checked_at"`
	Spec          string    `json:"spec"`
	// Host is the [hosts] entry applied, empty when none matched.
	Host       string    `json:"host"`
	Findings   []Finding `json:"findings"`
	Errors     int       `json:"errors"`
	Warnings   int       `json:"warnings"`
	MCPServers int       `json:"mcp_servers_read"`
	Hooks      int       `json:"hooks_read"`
	Plugins    int       `json:"plugins_read"`
}

// OK reports whether the machine matches the spec.
func (r Report) OK() bool { return r.Errors == 0 }

// FileChange is one file Apply changed, or would change.
type FileChange struct {
	Path    string   `json:"path"`
	Changes []string `json:"changes"`
	// Backup holds the previous content once written; empty for a new file.
	Backup string `json:"backup,omitempty"`
}

// ApplyReport is the result of Plan and Apply.
type ApplyReport struct {
	SchemaVersion int          `json:"schema_version"`
	AppliedAt     time.Time    `json:"applied_at"`
	Spec          string       `json:"spec"`
	Host          string       `json:"host"`
	Written       bool         `json:"written"`
	Files         []FileChange `json:"files"`
}

// Check compares the machine with the spec without changing anything.
func Check(ctx context.Context, opts Options) (Report, error) {
	opts, desired, err := prepare(ctx, opts)
	if err != nil {
		return Report{}, err
	}
	inv, err := inventory(opts.Home)
	if err != nil {
		return Report{}, err
	}
	report := Report{
		SchemaVersion: SchemaVersion, CheckedAt: time.Now().UTC(), Spec: opts.SpecPath, Host: desired.Host,
		Findings:   check.Run(desired, inv),
		MCPServers: len(inv.MCPServers), Hooks: len(inv.Hooks), Plugins: len(inv.Plugins),
	}
	if report.Findings == nil {
		report.Findings = []Finding{}
	}
	for _, f := range report.Findings {
		if f.Severity == Error {
			report.Errors++
		} else {
			report.Warnings++
		}
	}
	return report, nil
}

// Plan returns the changes Apply would make, without writing.
func Plan(ctx context.Context, opts Options) (ApplyReport, error) {
	opts, desired, err := prepare(ctx, opts)
	if err != nil {
		return ApplyReport{}, err
	}
	edits, err := plan(opts.Home, desired)
	if err != nil {
		return ApplyReport{}, err
	}
	return applyReport(opts, desired, edits, false), nil
}

// Apply writes the MCP servers declared in the spec and removes the hooks it
// does not allow. Every other key, including project trust and Codex's
// hooks.state, is kept. Running it again changes nothing.
func Apply(ctx context.Context, opts Options) (ApplyReport, error) {
	opts, desired, err := prepare(ctx, opts)
	if err != nil {
		return ApplyReport{}, err
	}
	runDir := filepath.Join(opts.BackupDir, time.Now().UTC().Format("20060102T150405.000000000Z"))

	// Agents rewrite their files at any time. When one does between planning
	// and writing, plan again from the new content rather than overwrite it.
	for attempt := 0; ; attempt++ {
		edits, err := plan(opts.Home, desired)
		if err != nil {
			return ApplyReport{}, err
		}
		report := applyReport(opts, desired, edits, true)
		err = write(edits, runDir, report.Files)
		if errors.Is(err, files.ErrChanged) && attempt < 2 {
			continue
		}
		if err != nil {
			return report, err
		}
		if len(edits) > 0 {
			if err := files.Prune(opts.BackupDir, backupsKept); err != nil {
				return report, err
			}
		}
		return report, nil
	}
}

func write(edits []model.FileEdit, runDir string, changes []FileChange) error {
	for i, edit := range edits {
		backup, err := files.Write(edit.Path, edit.Before, edit.After, runDir)
		if err != nil {
			return err
		}
		changes[i].Backup = backup
	}
	return nil
}

// WatchPaths are the files that, when changed, can change the result of Check
// or Apply for opts. The spec itself is not included.
func WatchPaths(opts Options) ([]string, error) {
	home, err := homeDir(opts.Home)
	if err != nil {
		return nil, err
	}
	return append(claude.WatchPaths(home), codex.WatchPaths(home)...), nil
}

// DefaultHostNames are the machine's hostname, without and with its domain,
// and its tailnet DNS name when the tailscale CLI can report it.
func DefaultHostNames(ctx context.Context) []string {
	var names []string
	add := func(name string) {
		name = strings.TrimSuffix(name, ".")
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if hostname, err := os.Hostname(); err == nil {
		short, _, _ := strings.Cut(hostname, ".")
		add(short)
		add(hostname)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "status", "--self", "--peers=false", "--json").Output()
	if err == nil {
		var status struct {
			Self struct{ DNSName string }
		}
		if json.Unmarshal(out, &status) == nil && status.Self.DNSName != "" {
			short, _, _ := strings.Cut(status.Self.DNSName, ".")
			add(short)
			add(status.Self.DNSName)
		}
	}
	return names
}

// SecretsPath is where the encrypted secrets live: next to the spec, so they
// travel through the same synced folder. A spec reached through a symlink,
// such as the default path linked to a synced folder, is followed first.
func SecretsPath(specPath string) string {
	if resolved, err := filepath.EvalSymlinks(specPath); err == nil {
		specPath = resolved
	}
	return filepath.Join(filepath.Dir(specPath), "secrets.age")
}

func prepare(ctx context.Context, opts Options) (Options, spec.Spec, error) {
	if opts.SpecPath == "" {
		return opts, spec.Spec{}, errors.New("no spec path")
	}
	home, err := homeDir(opts.Home)
	if err != nil {
		return opts, spec.Spec{}, err
	}
	opts.Home = home
	if opts.HostNames == nil {
		opts.HostNames = DefaultHostNames(ctx)
	}
	if opts.BackupDir == "" {
		state := os.Getenv("XDG_STATE_HOME")
		if state == "" {
			state = filepath.Join(home, ".local", "state")
		}
		opts.BackupDir = filepath.Join(state, "agentcfg", "backups")
	}
	file, err := spec.Load(opts.SpecPath)
	if err != nil {
		return opts, spec.Spec{}, fmt.Errorf("spec: %w", err)
	}
	desired, err := file.Resolve(opts.HostNames)
	if err != nil {
		return opts, spec.Spec{}, fmt.Errorf("spec: %w", err)
	}
	return opts, desired, nil
}

func homeDir(home string) (string, error) {
	if home != "" {
		return home, nil
	}
	return os.UserHomeDir()
}

func inventory(home string) (model.Inventory, error) {
	var inv model.Inventory
	for _, read := range []func(string) (model.Inventory, error){claude.Inventory, codex.Inventory} {
		agentInv, err := read(home)
		if err != nil {
			return inv, err
		}
		inv.Merge(agentInv)
	}
	return inv, nil
}

func plan(home string, desired spec.Spec) ([]model.FileEdit, error) {
	inv, err := inventory(home)
	if err != nil {
		return nil, err
	}
	claudeEdits, err := claude.Plan(home, desired, inv)
	if err != nil {
		return nil, err
	}
	codexEdits, err := codex.Plan(home, desired)
	if err != nil {
		return nil, err
	}
	return append(claudeEdits, codexEdits...), nil
}

func applyReport(opts Options, desired spec.Spec, edits []model.FileEdit, written bool) ApplyReport {
	report := ApplyReport{
		SchemaVersion: SchemaVersion, AppliedAt: time.Now().UTC(), Spec: opts.SpecPath, Host: desired.Host,
		Written: written && len(edits) > 0, Files: []FileChange{},
	}
	for _, edit := range edits {
		report.Files = append(report.Files, FileChange{Path: edit.Path, Changes: edit.Changes})
	}
	return report
}
