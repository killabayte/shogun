package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/provider"
)

// Dummy shell CLIs record only the presence of one synthetic credential. They neither open
// connections nor record the ambient environment, and no real provider executable is invoked.
func TestPR20ReviewLegacyCustomKeyStaysOutOfChildren(t *testing.T) {
	for _, source := range []string{"config", "snapshot"} {
		t.Run(source, func(t *testing.T) {
			ws := t.TempDir()
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", "")
			keyName := "REVIEW_ONLY_" + strings.ToUpper(source) + "_CREDENTIAL"
			t.Setenv(keyName, "synthetic-review-marker")
			observed := filepath.Join(ws, "presence.txt")
			t.Setenv("SHOGUN_REVIEW_PRESENCE_FILE", observed)
			features := strings.Join(provider.CodexDisabledFeatures, "\n") + "\n"
			bin := filepath.Join(ws, "fake-cli")
			script := "#!/bin/sh\n" +
				"if [ \"$1\" = --version ]; then printf 'review-cli-v1\\n'; exit 0; fi\n" +
				"if [ \"$1\" = features ]; then printf '%s' '" + features + "'; exit 0; fi\n" +
				"cat >/dev/null\nprintf '%s' \"${" + keyName + "+present}\" > \"$SHOGUN_REVIEW_PRESENCE_FILE\"\nexit 1\n"
			if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			contents := fmt.Sprintf("claude_command = %q\ncodex_command = %q\njev = \"off\"\njev_api_key_env = %q\n", bin, bin, keyName)
			path := filepath.Join(ws, ".shogun", "config.toml")
			if source == "snapshot" {
				path = filepath.Join(ws, "config.snapshot.toml")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			var cfg config.Config
			if source == "snapshot" {
				c, err := config.LoadSnapshot(path)
				if err != nil {
					t.Fatal(err)
				}
				cfg = *c
			} else {
				l, err := config.Load(ws, config.Overrides{}, os.Getenv)
				if err != nil {
					t.Fatal(err)
				}
				cfg = l.Config
			}
			items := preflightItems(cfg, bin, "review-cli-v1", bin, "review-cli-v1", features, os.Getenv)
			fp := provider.Fingerprint(items)
			record := &provider.Preflight{Version: provider.PreflightVersion, Fingerprint: fp, Items: items}
			for _, c := range []string{"claude/call", "claude/reads", "claude/no-write", "codex/call", "codex/reads", "codex/write-denied", "codex/no-write"} {
				p, n, _ := strings.Cut(c, "/")
				record.Checks = append(record.Checks, provider.Check{Provider: p, Name: n, Verdict: "pass"})
			}
			rp := preflightPath(ws, fp)
			if err := os.MkdirAll(filepath.Dir(rp), 0o700); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(rp, data, 0o600); err != nil {
				t.Fatal(err)
			}
			a := &app{ctx: context.Background(), cwd: ws, getenv: os.Getenv}
			planner, reviewer, _, err := a.runners(a.ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range []struct {
				name   string
				runner provider.Runner
			}{{"claude", planner}, {"codex", reviewer}} {
				os.Remove(observed)
				_, _ = r.runner.Run(a.ctx, provider.Request{Dir: filepath.Join(ws, r.name+"-call"), Model: "fake-model", Effort: "high", Prompt: "offline", Schema: []byte(`{"type":"object"}`), Roots: []string{ws}, MaxAttempts: 1})
				b, err := os.ReadFile(observed)
				if err != nil {
					t.Fatalf("fake CLI did not run: %v", err)
				}
				if len(b) != 0 {
					t.Errorf("%s: legacy custom credential reached %s child", source, r.name)
				}
			}
		})
	}
}
