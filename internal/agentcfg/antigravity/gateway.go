// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package antigravity

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"runtime"
	"slices"

	"github.com/syncthing/syncthing/internal/agentcfg/check"
	"github.com/syncthing/syncthing/internal/agentcfg/model"
	"github.com/syncthing/syncthing/internal/agentcfg/secrets"
	"github.com/syncthing/syncthing/internal/agentcfg/spec"
)

const (
	gatewayHeader = "# Written by agentcfg apply from [antigravity.gateway] in the spec. Do not edit or sync this file.\n"
	gatewayMode   = fs.FileMode(0o600)
)

// planGateway computes the gateway environment file. agy reads the gateway
// only from AGY_LLM_GATEWAY_* variables (or a root-owned admin file), so
// apply writes them, with the API key taken from lookup, into an unsynced
// environment.d file with mode 0600. Without a gateway in the spec, an
// existing file is emptied rather than left pointing at the old one. missing names the
// variable lookup has no value for; the file is then left as it is.
func planGateway(path string, gateway *spec.Gateway, lookup func(string) (string, bool)) (edit *model.FileEdit, missing []string, err error) {
	before, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		before = nil
	} else if err != nil {
		return nil, nil, err
	}

	var env map[string]string
	change := "stop sending model requests to a gateway"
	if gateway != nil {
		env, missing = gateway.Environment(lookup)
		if len(missing) > 0 {
			return nil, missing, nil
		}
		// The key comes from the environment, so it is checked here; the
		// name, not the value, goes into the error.
		if err := secrets.ValidateValue("AGY_LLM_GATEWAY_API_KEY", env["AGY_LLM_GATEWAY_API_KEY"]); err != nil {
			return nil, nil, err
		}
		change = "send model requests to the gateway " + gateway.URL
	} else if before == nil {
		return nil, nil, nil
	}

	var after bytes.Buffer
	after.WriteString(gatewayHeader)
	for _, name := range slices.Sorted(maps.Keys(env)) {
		fmt.Fprintf(&after, "%s='%s'\n", name, env[name])
	}
	// The file holds the key, so it must not be readable by others even if
	// something else created it. Windows has no such mode bits.
	wrongMode := false
	if before != nil && runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			return nil, nil, err
		}
		wrongMode = info.Mode().Perm() != gatewayMode
	}
	if bytes.Equal(before, after.Bytes()) {
		if !wrongMode {
			return nil, nil, nil
		}
		change = "set the mode to 0600"
	}
	return &model.FileEdit{Path: path, Before: before, After: after.Bytes(), Mode: gatewayMode, Changes: []string{change}}, nil, nil
}

// GatewayFindings reports a gateway environment file that differs from
// what apply would write, and a missing value for its API key.
func GatewayFindings(home string, desired spec.Spec, inv model.Inventory, lookup func(string) (string, bool)) ([]check.Finding, error) {
	if slices.Contains(inv.Absent, model.Antigravity) {
		return nil, nil
	}
	path := PathsFor(home).GatewayEnv
	edit, missing, err := planGateway(path, desired.Gateway, lookup)
	if err != nil {
		return nil, err
	}
	var findings []check.Finding
	for _, variable := range missing {
		findings = append(findings, check.Finding{Severity: check.Warning, Agent: model.Antigravity, Rule: "unset-variable", Source: path, Location: "antigravity.gateway.api_key", Message: variable + " is not set in this environment, so apply cannot write the gateway"})
	}
	if edit != nil {
		findings = append(findings, check.Finding{Severity: check.Error, Agent: model.Antigravity, Rule: "gateway-mismatch", Source: path, Location: "antigravity.gateway", Message: "the gateway environment differs from the spec, or its mode is not 0600"})
	}
	return findings, nil
}
