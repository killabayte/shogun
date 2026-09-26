package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Execute each script for real. Neither script attempts to open the target for
// writing: the apparent redirect is in a comment or an unexecuted && branch.
type p2Round3NoWriteRunner struct{ mode string }

func (r p2Round3NoWriteRunner) Run(ctx context.Context, req Request) (*Result, error) {
	target := filepath.Join(req.Roots[0], "PROBE_WRITE.txt")
	print := fmt.Sprintf(`printf "operation not permitted: %%s\n" %q`, target)
	script := print + fmt.Sprintf(`; # > %q`, target)
	if r.mode == "skipped_branch" {
		script = fmt.Sprintf(`false && printf probe > %q; `, target) + print
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		return nil, fmt.Errorf("target must be absent before control: %v", err)
	}
	out, err := exec.CommandContext(ctx, "/bin/sh", "-c", script).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("control script failed: %w: %s", err, out)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		return nil, fmt.Errorf("control unexpectedly created target: %v", err)
	}
	line, err := json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"type":              "command_execution",
			"command":           script,
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

func TestP2Round3PreflightRejectsUnexecutedRedirect(t *testing.T) {
	for _, mode := range []string{"comment", "skipped_branch"} {
		t.Run(mode, func(t *testing.T) {
			checks := RunPreflight(context.Background(), stubRunner{}, p2Round3NoWriteRunner{mode: mode}, Request{}, Request{}, t.TempDir())
			got := verdicts(checks)
			for _, name := range requiredChecks {
				if name != "codex/write-denied" && got[name] != "pass" {
					t.Fatalf("control failed for %s: %v", name, got)
				}
			}
			if got["codex/write-denied"] == "pass" {
				t.Errorf("unexecuted redirect certified as a denied write: %v", got)
			}
			record := &Preflight{Version: PreflightVersion, Fingerprint: "profile", Checks: checks}
			if err := record.Verify("profile"); err == nil {
				t.Error("entire profile certified without an attempted write")
			}
		})
	}
}
