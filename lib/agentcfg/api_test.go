// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package agentcfg

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	agentcfglib "github.com/torturado/agentcfg"

	"github.com/syncthing/syncthing/lib/config"
)

func request(t *testing.T, h http.Handler, method, target string, into any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	if into != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatalf("%s %s: %v in %s", method, target, err, rec.Body)
		}
	}
	return rec.Code
}

func TestRESTEndpoints(t *testing.T) {
	env := newTestEnv(t, nil)
	writeFile(t, filepath.Join(env.folder, SpecFileName), testSpec)
	claudeJSON := filepath.Join(env.home, ".claude.json")

	var plan agentcfglib.ApplyReport
	if code := request(t, env.svc, http.MethodPost, "/rest/agentcfg/apply?dryrun=true", &plan); code != http.StatusOK || plan.Written || len(plan.Files) != 1 {
		t.Errorf("dry run = %d, %+v; want one file to change, nothing written", code, plan)
	}
	if got := readFile(t, claudeJSON); got != "{}" {
		t.Errorf("dry run wrote %q", got)
	}

	// An explicit apply writes even with automatic apply off, and checks
	// again.
	var applied agentcfglib.ApplyReport
	if code := request(t, env.svc, http.MethodPost, "/rest/agentcfg/apply", &applied); code != http.StatusOK || !applied.Written {
		t.Errorf("apply = %d, %+v; want written", code, applied)
	}
	if !strings.Contains(readFile(t, claudeJSON), "search.example.com") {
		t.Error("apply did not write the server")
	}
	var st Status
	if code := request(t, env.svc, http.MethodGet, "/rest/agentcfg/report", &st); code != http.StatusOK || reportErrors(st) != 0 || st.LastApply == nil || !st.LastApply.Written {
		t.Errorf("report = %d, %+v; want no errors and the apply", code, st)
	}

	// Without the fleet option there is no fleet view.
	if code := request(t, env.svc, http.MethodGet, "/rest/agentcfg/fleet", nil); code != http.StatusNotFound {
		t.Errorf("fleet while disabled = %d, want 404", code)
	}

	w, err := env.cfg.Modify(func(cfg *config.Configuration) { cfg.Options.AgentcfgEnabled = false })
	if err != nil {
		t.Fatal(err)
	}
	w.Wait()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		target := "/rest/agentcfg/report"
		if method == http.MethodPost {
			target = "/rest/agentcfg/apply"
		}
		if code := request(t, env.svc, method, target, nil); code != http.StatusNotFound {
			t.Errorf("%s %s while disabled = %d, want 404", method, target, code)
		}
	}
}

func TestRESTApplyReportsMissingSpec(t *testing.T) {
	env := newTestEnv(t, nil)
	if code := request(t, env.svc, http.MethodPost, "/rest/agentcfg/apply", nil); code != http.StatusInternalServerError {
		t.Errorf("apply without a spec = %d, want 500", code)
	}
}
