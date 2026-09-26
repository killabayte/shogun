package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/killabayte/shogun/internal/inputs"
	"github.com/killabayte/shogun/internal/provider"
	"github.com/killabayte/shogun/internal/run"
)

func TestP3Round2ExplicitSourceCannotBecomeSupporting(t *testing.T) {
	for _, verdict := range []string{"unverified", "contradicted"} {
		t.Run(verdict, func(t *testing.T) {
			d := research(r1)
			d["source_coverage"] = append(d["source_coverage"].([]any), map[string]any{"source_id": "in-1", "studied": "policy.md", "relevance": "relevant", "notes": "mandatory policy"})
			r := review("approve", researchOK, nil)
			r["source_assessments"] = []any{map[string]any{"source_id": "in-1", "role": "supporting", "verdict": verdict, "note": "The proposed behaviour does not agree with the supplied policy."}}
			f := newFixture(t,
				[]reply{fixed(d), fixed(outline(step{"S-001", []string{"R-001.C1"}}))},
				[]reply{fixed(r), fixed(review("approve", outlineOK, nil))})
			f.e.Cfg.ReviewRounds = 1
			if err := f.e.Run.WriteArtifact("inputs/policy.md", []byte("Preserve the public API.\n")); err != nil {
				t.Fatal(err)
			}
			f.e.Manifest.Inputs = []inputs.Source{{ID: "in-1", Origin: "policy.md", StoredPath: "inputs/policy.md", Status: "ok"}}
			o := f.execute(t)
			if f.e.State.Progress.Approved[StageResearch] != 0 {
				t.Fatalf("explicit input labelled supporting bypassed source gate: verdict=%s outcome=%+v approvals=%v", verdict, o, f.e.State.Progress.Approved)
			}
		})
	}
}

func TestP3Round2BudgetPreventingFormatCorrectionPauses(t *testing.T) {
	f := newFixture(t, nil, nil)
	dir := t.TempDir()
	bin, counter := filepath.Join(dir, "fake-claude"), filepath.Join(dir, "attempts")
	// A completed response has an invalid research payload. With one attempt
	// left, the normal single format-correction attempt cannot be started.
	body := "#!/bin/sh\n/bin/cat >/dev/null\nprintf x >> '" + counter + "'\n" +
		`printf '%s\n' '{"type":"system","subtype":"init","model":"claude-fable-5-1","permissionMode":"dontAsk","tools":["Read","Grep","Glob","StructuredOutput"]}' '{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","structured_output":{}}'` + "\n"
	if err := os.WriteFile(bin, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	f.e.Planner = &provider.Claude{Bin: bin}
	f.e.State.Limits.MaxAttempts, f.e.State.Limits.Source = 1, "flag"
	o := f.execute(t)
	b, err := os.ReadFile(counter)
	if err != nil || len(b) != 1 || f.e.State.Counters.Attempts != 1 {
		t.Fatalf("attempt-cap control failed: processes=%d attempts=%d error=%v", len(b), f.e.State.Counters.Attempts, err)
	}
	if o.Status != run.StatusPaused || !strings.HasPrefix(o.Reason, "limit:") {
		t.Fatalf("budget prevented format correction but run was not paused on its limit: %+v", o)
	}
}
