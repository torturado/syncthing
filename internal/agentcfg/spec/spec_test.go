// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package spec_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/syncthing/syncthing/internal/agentcfg/spec"
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
		"unknown key":                   "[mcp_servers.search]\nurl = \"https://example.com\"\ntimeout = 5\n",
		"literal credential":            "[mcp_servers.search]\nurl = \"https://example.com\"\nheaders = { Authorization = \"Bearer abc\" }\n",
		"header Codex cannot reference": "[mcp_servers.search]\nurl = \"https://example.com\"\nheaders = { X-Key = \"key ${KEY}\" }\n",
		"renamed variable for Codex":    "[mcp_servers.files]\ncommand = \"mcp-files\"\nenv = { FILES_TOKEN = \"${OTHER}\" }\n",
		"url and command":               "[mcp_servers.search]\nurl = \"https://example.com\"\ncommand = \"x\"\n",
		"unknown role":                  "[hosts.desktop]\nroles = [\"build\"]\n",
		"invalid age recipient":         "[hosts.desktop]\nage_recipient = \"age1notakey\"\n",
		"codex plugins":                 "[plugins]\ncodex = []\n",
		"server without target":         "[mcp_servers.search]\nagents = [\"claude\"]\n",
		"unknown server agent":          "[mcp_servers.search]\nurl = \"https://example.com\"\nagents = [\"cursor\"]\n",
		"unknown hook agent":            "[hooks]\ncursor = []\n",
		"unknown sync folder":           "[sync.exclude]\ncursor = [\"/rules\"]\n",
		"sync inclusion":                "[sync.exclude]\nclaude = [\"(?i)!/.credentials.json\"]\n",
		"sync directive":                "[sync.exclude]\nagents = [\"#include other\"]\n",
		"sync line break":               "[sync.exclude]\nagents = [\"/a\\n!/b\"]\n",
		"sync blank pattern":            "[sync.exclude]\nagents = [\" \"]\n",
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
	err := load(t, "[mcp_servers.search]\nurl = \"https://example.com\"\n\n[mcp_servers.files]\ncommand = \"mcp-files\"\nagents = [\"claude\"]\n\n[hooks]\nclaude = [\"notify\"]\n\n[sync.exclude]\nagents = [\"/skills/local\", \"(?d)**/*.tmp\"]\n")
	if err != nil {
		t.Fatal(err)
	}
}
