package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
)

// Run an actual successful printf, without attempting a write. Its output mentions
// the probe path, as a model quoting an expected sandbox diagnostic could do.
type p2Round2EchoRunner struct{}

func (p2Round2EchoRunner) Run(ctx context.Context, req Request) (*Result, error) {
	target := filepath.Join(req.Roots[0], "PROBE_WRITE.txt")
	diagnostic := "operation not permitted: " + target
	out, err := exec.CommandContext(ctx, "/usr/bin/printf", "%s\n", diagnostic).Output()
	if err != nil {
		return nil, fmt.Errorf("echo control: %w", err)
	}
	line, err := json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"type":              "command_execution",
			"command":           fmt.Sprintf("printf '%%s\\n' %q", diagnostic),
			"aggregated_output": string(out),
			"exit_code":         0,
			"status":            "completed",
		},
	})
	if err != nil {
		return nil, err
	}
	return (stubRunner{stdout: string(line) + "\n"}).Run(ctx, req)
}

func TestP2Round2PreflightRejectsSuccessfulTargetEcho(t *testing.T) {
	checks := RunPreflight(context.Background(), stubRunner{}, p2Round2EchoRunner{}, Request{}, Request{}, t.TempDir())
	got := verdicts(checks)
	for _, name := range requiredChecks {
		if name != "codex/write-denied" && got[name] != "pass" {
			t.Fatalf("control failed for %s: %v", name, got)
		}
	}
	if got["codex/write-denied"] == "pass" {
		t.Errorf("successful printf with the probe path certified as a denied write: %v", got)
	}
	record := &Preflight{Version: PreflightVersion, Fingerprint: "profile", Checks: checks}
	if err := record.Verify("profile"); err == nil {
		t.Error("entire profile certified without any attempted write")
	}
}
