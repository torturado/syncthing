// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package agentcfg

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// ServeHTTP serves the agentcfg endpoints of the REST API. lib/api routes
// them to it behind the same authentication and CSRF protection as the
// rest of the API.
//
//	GET  /rest/agentcfg/report               this device's Status
//	GET  /rest/agentcfg/fleet                []FleetMember
//	POST /rest/agentcfg/apply[?dryrun=true]  the ApplyReport of Apply or Plan
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/rest/agentcfg/report":
		if !s.cfg.Options().AgentcfgEnabled {
			http.Error(w, errDisabled.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, s.Status())

	case "/rest/agentcfg/fleet":
		fleet, err := s.Fleet(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, fleet)

	case "/rest/agentcfg/apply":
		dryRun, _ := strconv.ParseBool(r.URL.Query().Get("dryrun"))
		report, err := s.Apply(r.Context(), dryRun)
		switch {
		case errors.Is(err, errDisabled):
			http.Error(w, err.Error(), http.StatusNotFound)
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			writeJSON(w, report)
		}

	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	bs, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(bs, '\n'))
}
