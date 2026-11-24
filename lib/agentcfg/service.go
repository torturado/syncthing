// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Package agentcfg runs internal/agentcfg inside Syncthing: it
// shares the directories of the AI coding agents' configuration with the
// devices marked for it, checks that configuration against a spec that
// travels in one of them, corrects it when asked to, and serves the report
// to the GUI and, when enabled, shares it with the other devices.
//
// The security behaviour is specified in
// docs/tailscale/agentcfg-threat-model.md.
package agentcfg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thejerf/suture/v4"

	agentcfglib "github.com/syncthing/syncthing/internal/agentcfg"
	"github.com/syncthing/syncthing/internal/db"
	"github.com/syncthing/syncthing/internal/slogutil"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/svcutil"
)

const (
	// SpecFileName is the spec looked for at the root of the shared folders
	// when no spec path is configured.
	SpecFileName = "agentcfg.toml"

	// A burst of triggers runs once, debounceQuiet after the last of them,
	// and at most debounceMax after the first.
	debounceQuiet = 2 * time.Second
	debounceMax   = 10 * time.Second

	defaultIntervalS = 600
	minIntervalS     = 60
)

var (
	errDisabled = errors.New("agentcfg is disabled")
	errNoSpec   = errors.New("no shared folder holds " + SpecFileName + " at its root; share one or set agentcfgSpecPath")
)

// Status is what the service knows about this device's agent
// configuration. It is served as JSON.
type Status struct {
	// Spec is the spec checked against, and Folder the ID of the shared
	// folder that holds it, if any.
	Spec   string `json:"spec"`
	Folder string `json:"folder"`
	// Report is the last check, or nil when it could not be made; Error
	// says why.
	Report *agentcfglib.Report `json:"report"`
	Error  string              `json:"error"`
	// LastApply is the last apply that wrote or failed, and ApplyError why
	// the last apply failed or why automatic apply did not run.
	LastApply  *agentcfglib.ApplyReport `json:"lastApply"`
	ApplyError string                   `json:"applyError"`
	// SyncError says why a folder of agent configuration is not shared
	// with the devices marked for it.
	SyncError string `json:"syncError"`
}

// Service shares the folders of agent configuration with the devices
// marked agentcfgSync and checks that configuration while agentcfgEnabled
// is set, and shares the report while agentcfgFleetEnabled is also set. It
// is a suture service.
type Service struct {
	*suture.Supervisor

	cfg      config.Wrapper
	evLogger events.Logger
	model    Model

	// Fixed outside tests.
	home      string // empty for the user's home
	hostNames func(context.Context) []string

	checksChanged chan struct{}

	// runMut serializes checks and applies; stamps is what the watched
	// files looked like before the last check.
	runMut sync.Mutex
	stamps map[string]fileStamp

	mut       sync.Mutex
	status    Status
	signature string

	// shareMut guards the shared report: the file written, what it said,
	// and why it could not be shared.
	shareMut  sync.Mutex
	shared    sharedFile
	sharedSig string
	shareErr  string
}

// Model is the part of the model that the service uses: it tells it about
// the files it writes into folders, and asks what the other devices offer.
type Model interface {
	ScanFolderSubdirs(folder string, subs []string) error
	PendingFolders(device protocol.DeviceID) (map[string]db.PendingFolder, error)
}

// New returns the agentcfg service. model may be nil, which leaves what the
// service writes to the folders' watchers and rescans.
func New(cfg config.Wrapper, evLogger events.Logger, model Model) *Service {
	s := &Service{
		Supervisor:    suture.New("agentcfg.Service", svcutil.SpecWithDebugLogger()),
		cfg:           cfg,
		evLogger:      evLogger,
		model:         model,
		hostNames:     agentcfglib.DefaultHostNames,
		checksChanged: make(chan struct{}, 1),
		signature:     signature(Status{}),
	}
	s.Add(svcutil.AsService(s.serveChecks, fmt.Sprintf("%s/serveChecks", s)))
	return s
}

// Status returns the current status.
func (s *Service) Status() Status {
	s.mut.Lock()
	defer s.mut.Unlock()
	return s.status
}

func (s *Service) serveChecks(ctx context.Context) error {
	s.cfg.Subscribe(s)
	defer s.cfg.Unsubscribe(s)
	for {
		if s.cfg.Options().AgentcfgEnabled {
			s.follow(ctx)
		}
		if ctx.Err() != nil {
			return nil
		}
		s.publish(Status{})
		s.share(Status{})
		select {
		case <-ctx.Done():
			return nil
		case <-s.checksChanged:
		}
	}
}

// trigger says why a run was asked for.
type trigger int

const (
	triggerNone trigger = iota
	// A watched agent file changed; this alone runs only if the files
	// differ from what the last run left, so that our own writes do not
	// trigger another run.
	triggerFile
	// Anything else: the spec, the configuration or the interval.
	triggerAlways
)

// follow runs checks on triggers until the context ends or agentcfg is
// disabled.
func (s *Service) follow(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The events are read beside the loop, which waits for runs: a
	// subscriber that falls behind delays the event logger for everyone.
	var target atomic.Pointer[specTarget]
	specChanged := make(chan struct{}, 1)
	sub := s.evLogger.Subscribe(events.LocalIndexUpdated | events.ItemFinished)
	defer sub.Unsubscribe()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-sub.C():
				if target.Load().concerns(ev) {
					select {
					case specChanged <- struct{}{}:
					default:
					}
				}
			}
		}
	}()

	watch := newFileWatch()
	defer watch.stop()

	interval := s.interval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	debounce := time.NewTimer(0)
	debounce.Stop()
	var pending trigger
	var first time.Time
	schedule := func(t trigger) {
		now := time.Now()
		if pending == triggerNone {
			first = now
		}
		pending = max(pending, t)
		debounce.Reset(min(debounceQuiet, debounceMax-now.Sub(first)))
	}

	current, files := s.refresh(ctx)
	target.Store(&current)
	watch.set(files)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.checksChanged:
			if !s.cfg.Options().AgentcfgEnabled {
				return
			}
			if next := s.interval(); next != interval {
				interval = next
				ticker.Reset(interval)
			}
			schedule(triggerAlways)
		case <-specChanged:
			schedule(triggerAlways)
		case info := <-watch.events:
			if watch.matches(info.Path()) {
				schedule(triggerFile)
			}
		case <-ticker.C:
			schedule(triggerAlways)
		case <-debounce.C:
			run := pending == triggerAlways || s.changedSinceRun()
			pending = triggerNone
			if run {
				current, files := s.refresh(ctx)
				target.Store(&current)
				watch.set(files)
			}
		}
	}
}

// interval is the time between runs when nothing triggers one sooner. It is
// only a backstop to the triggers.
func (s *Service) interval() time.Duration {
	seconds := s.cfg.Options().AgentcfgIntervalS
	if seconds == 0 {
		seconds = defaultIntervalS
	}
	return time.Duration(max(seconds, minIntervalS)) * time.Second
}

// refresh keeps the folders of agent configuration, applies the spec when
// automatic apply is on, then checks, and publishes the result. It returns
// the spec it used and the files to watch.
func (s *Service) refresh(ctx context.Context) (specTarget, []string) {
	s.runMut.Lock()
	defer s.runMut.Unlock()

	var syncErr error
	switch target, err := findSpec(s.cfg.RawCopy()); {
	case err == nil:
		syncErr = s.syncFolders(target.path)
	case errors.Is(err, errNoSpec):
		// No exclusions until the spec arrives, as on a new device.
		syncErr = s.syncFolders("")
	default:
		syncErr = fmt.Errorf("the spec that says what to exclude: %w", err)
	}
	if syncErr != nil {
		slog.WarnContext(ctx, "Failed to share the agent configuration folders", slogutil.Error(syncErr))
	}

	// The folders just kept may hold the spec.
	cfg := s.cfg.RawCopy()
	target, err := findSpec(cfg)
	prev := s.Status()
	st := Status{Spec: target.path, Folder: target.folder, LastApply: prev.LastApply}
	if syncErr != nil {
		st.SyncError = syncErr.Error()
	}
	if errors.Is(err, errNoSpec) {
		// Nothing to check against yet, as on a device that has not received
		// the spec, which is not an error.
		return target, s.checkLocked(ctx, st, agentcfglib.Options{Home: s.home})
	}
	if err != nil {
		st.Error = err.Error()
		return target, s.checkLocked(ctx, st, agentcfglib.Options{Home: s.home})
	}
	opts := agentcfglib.Options{SpecPath: target.path, Home: s.home, HostNames: s.hostNames(ctx)}
	if cfg.Options.AgentcfgAutoApply() {
		if !target.explicit && target.folder != agentsFolderID {
			st.ApplyError = "automatic apply needs agentcfgSpecPath, or the spec at the root of " + agentsFolderID + "; a spec found in another shared folder is only checked"
		} else {
			report, err := agentcfglib.Apply(ctx, opts)
			if err != nil {
				slog.WarnContext(ctx, "Failed to apply the agent configuration spec", slogutil.FilePath(target.path), slogutil.Error(err))
				st.ApplyError = err.Error()
				st.LastApply = &report
			} else if report.Written {
				slog.InfoContext(ctx, "Applied the agent configuration spec", slogutil.FilePath(target.path), slog.Any("files", writtenPaths(report)))
				st.LastApply = &report
			}
		}
	}
	return target, s.checkLocked(ctx, st, opts)
}

// Apply writes the spec, or with dryRun returns what it would write, at
// the owner's request: it runs whatever agentcfgApply says.
func (s *Service) Apply(ctx context.Context, dryRun bool) (agentcfglib.ApplyReport, error) {
	if !s.cfg.Options().AgentcfgEnabled {
		return agentcfglib.ApplyReport{}, errDisabled
	}
	s.runMut.Lock()
	defer s.runMut.Unlock()

	target, err := findSpec(s.cfg.RawCopy())
	if err != nil {
		return agentcfglib.ApplyReport{}, err
	}
	opts := agentcfglib.Options{SpecPath: target.path, Home: s.home, HostNames: s.hostNames(ctx)}
	if dryRun {
		return agentcfglib.Plan(ctx, opts)
	}
	report, err := agentcfglib.Apply(ctx, opts)
	st := Status{Spec: target.path, Folder: target.folder, LastApply: &report}
	if err != nil {
		st.ApplyError = err.Error()
	} else if report.Written {
		slog.InfoContext(ctx, "Applied the agent configuration spec on request", slogutil.FilePath(target.path), slog.Any("files", writtenPaths(report)))
	}
	s.checkLocked(ctx, st, opts)
	return report, err
}

// checkLocked records how the watched files look, then checks unless st
// already holds an error or there is no spec, and publishes st. It returns
// the watched files.
func (s *Service) checkLocked(ctx context.Context, st Status, opts agentcfglib.Options) []string {
	files, err := watchFiles(opts)
	if err != nil {
		slog.WarnContext(ctx, "Failed to list the agent configuration files", slogutil.Error(err))
	}
	// Recorded before checking, so that a write the check does not see
	// still differs from the record.
	s.stamps = stampFiles(files)
	if st.Error == "" && opts.SpecPath != "" {
		report, err := agentcfglib.Check(ctx, opts)
		if err != nil {
			st.Error = err.Error()
		} else {
			st.Report = &report
		}
	}
	s.publish(st)
	s.share(st)
	return files
}

// changedSinceRun reports whether the watched files differ from what they
// were before the last check.
func (s *Service) changedSinceRun() bool {
	s.runMut.Lock()
	defer s.runMut.Unlock()
	return s.stamps == nil || !maps.Equal(s.stamps, stampFiles(slices.Collect(maps.Keys(s.stamps))))
}

// publish stores the status, and logs and announces it when the result
// changed, rather than on every run.
func (s *Service) publish(st Status) {
	sig := signature(st)
	s.mut.Lock()
	s.status = st
	changed := sig != s.signature
	s.signature = sig
	s.mut.Unlock()
	if !changed {
		return
	}

	data := map[string]any{"error": st.Error, "applyError": st.ApplyError}
	switch {
	case st.Error != "":
		slog.Warn("Failed to check the agent configuration", slogutil.Error(st.Error))
	case st.Report == nil:
		// Disabled.
	case st.Report.OK():
		slog.Info("Agent configuration matches the spec", slogutil.FilePath(st.Spec))
	default:
		slog.Warn("Agent configuration differs from the spec", slogutil.FilePath(st.Spec), slog.Int("errors", st.Report.Errors), slog.Int("warnings", st.Report.Warnings))
	}
	if st.Report != nil {
		data["ok"] = st.Report.OK()
		data["errors"] = st.Report.Errors
		data["warnings"] = st.Report.Warnings
	}
	s.evLogger.Log(events.AgentConfigChanged, data)
}

// signature identifies what a status says, regardless of when it was made
// and of the order of its findings.
func signature(st Status) string {
	parts := []string{st.Spec, st.Error, st.ApplyError, st.SyncError}
	if st.Report != nil {
		findings := make([]string, len(st.Report.Findings))
		for i, f := range st.Report.Findings {
			findings[i] = strings.Join([]string{string(f.Severity), string(f.Agent), f.Rule, f.Source, f.Location, f.Message}, "\x00")
		}
		slices.Sort(findings)
		parts = append(parts, "report")
		parts = append(parts, findings...)
	}
	return strings.Join(parts, "\x01")
}

func writtenPaths(report agentcfglib.ApplyReport) []string {
	paths := make([]string, len(report.Files))
	for i, f := range report.Files {
		paths[i] = f.Path
	}
	return paths
}

func (s *Service) CommitConfiguration(from, to config.Configuration) bool {
	if !sameSettings(from, to) {
		select {
		case s.checksChanged <- struct{}{}:
		default:
		}
	}
	return true
}

// sameSettings reports whether two configurations run agentcfg the same
// way: the same options, the same devices to share the agent configuration
// with, and the same folders to find the spec in and keep.
func sameSettings(a, b config.Configuration) bool {
	ao, bo := a.Options, b.Options
	if ao.AgentcfgEnabled != bo.AgentcfgEnabled || ao.AgentcfgSpecPath != bo.AgentcfgSpecPath ||
		ao.AgentcfgApply != bo.AgentcfgApply || ao.AgentcfgIntervalS != bo.AgentcfgIntervalS ||
		ao.AgentcfgFleetEnabled != bo.AgentcfgFleetEnabled {
		return false
	}
	if !slices.Equal(syncPeers(a, protocol.EmptyDeviceID), syncPeers(b, protocol.EmptyDeviceID)) {
		return false
	}
	return slices.EqualFunc(a.Folders, b.Folders, func(x, y config.FolderConfiguration) bool {
		return x.ID == y.ID && x.Path == y.Path && x.FilesystemType == y.FilesystemType && x.Type == y.Type &&
			(!strings.HasPrefix(x.ID, SyncFolderPrefix) || slices.Equal(x.DeviceIDs(), y.DeviceIDs()))
	})
}

func (s *Service) String() string {
	return fmt.Sprintf("agentcfg.Service@%p", s)
}

// specTarget is the spec in use and where it lives.
type specTarget struct {
	path     string
	folder   string // ID of the shared folder that holds it, if any
	name     string // its name within that folder
	explicit bool   // named by agentcfgSpecPath, rather than found
}

// findSpec returns the configured spec, or the only agentcfg.toml at the
// root of a shared folder.
func findSpec(cfg config.Configuration) (specTarget, error) {
	type root struct{ id, path string }
	var roots []root
	for _, f := range cfg.Folders {
		// The content of a receive-encrypted folder is not readable here.
		if f.FilesystemType != config.FilesystemTypeBasic || f.Type == config.FolderTypeReceiveEncrypted {
			continue
		}
		if path, err := fs.ExpandTilde(f.Path); err == nil {
			roots = append(roots, root{f.ID, filepath.Clean(path)})
		}
	}

	if path := cfg.Options.AgentcfgSpecPath; path != "" {
		path, err := fs.ExpandTilde(path)
		if err != nil {
			return specTarget{}, fmt.Errorf("agentcfgSpecPath: %w", err)
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return specTarget{}, fmt.Errorf("agentcfgSpecPath: %w", err)
		}
		target := specTarget{path: path, explicit: true}
		// The innermost folder holding the spec is the one that syncs it.
		for _, r := range roots {
			rel, err := filepath.Rel(r.path, path)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			if target.folder == "" || len(rel) < len(target.name) {
				target.folder, target.name = r.id, rel
			}
		}
		return target, nil
	}

	var found []specTarget
	for _, r := range roots {
		path := filepath.Join(r.path, SpecFileName)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			found = append(found, specTarget{path: path, folder: r.id, name: SpecFileName})
		}
	}
	switch len(found) {
	case 0:
		return specTarget{}, errNoSpec
	case 1:
		return found[0], nil
	default:
		ids := make([]string, len(found))
		for i, t := range found {
			ids[i] = t.folder
		}
		return specTarget{}, fmt.Errorf("several shared folders hold %s at their root (%s); set agentcfgSpecPath", SpecFileName, strings.Join(ids, ", "))
	}
}

// concerns reports whether a LocalIndexUpdated or ItemFinished event is
// about the spec. While no single spec is found, any folder's root spec
// concerns it.
func (t specTarget) concerns(ev events.Event) bool {
	data, ok := ev.Data.(map[string]any)
	if !ok {
		return false
	}
	folder, _ := data["folder"].(string)
	var names []string
	switch ev.Type {
	case events.LocalIndexUpdated:
		names, _ = data["filenames"].([]string)
	case events.ItemFinished:
		if name, ok := data["item"].(string); ok {
			names = []string{name}
		}
	}
	if t.path == "" {
		return slices.Contains(names, SpecFileName)
	}
	if t.folder == "" || folder != t.folder {
		return false
	}
	return slices.ContainsFunc(names, func(name string) bool {
		return filepath.FromSlash(name) == t.name
	})
}
