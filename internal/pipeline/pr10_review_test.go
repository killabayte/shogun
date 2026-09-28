package pipeline

import (
	"testing"
	"time"

	"github.com/killabayte/shogun/internal/library"
	"github.com/killabayte/shogun/internal/planning/schema"
	"github.com/killabayte/shogun/internal/run"
)

// Resolving the obsolete damaged-run shape does not authorize ignoring an unrelated
// contradiction or a newly unverified claim in the same immutable input.
func TestPR10ReviewSourceAnswerIsNotASourceWideWaiver(t *testing.T) {
	for _, tc := range []struct {
		name, verdict, note string
	}{
		{"different_conflict", "contradicted", "A separate read-only requirement in draft.md conflicts with the candidate, which writes state.json. This was not part of the damaged-run shape decision."},
		{"unverified", "unverified", "The input's independent read-only requirement has not been checked against the candidate. The prior decision only resolves the damaged-run shape."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fastWithContradictedInput(t)
			f.planner.replies = append(f.planner.replies, f.planner.replies[0])
			rev := finalReview("approve", nil, s1)
			rev["source_assessments"] = []any{map[string]any{"source_id": "in-1", "role": "explicit", "verdict": tc.verdict, "note": tc.note}}
			f.reviewer.replies = append(f.reviewer.replies, fixed(rev))
			if o := f.execute(t); o.Status != run.StatusNeedsInput || len(f.e.State.Progress.Pending) != 1 || f.e.State.Counters.Attempts != 2 {
				t.Fatalf("expected the initial source question after two attempts: %+v", o)
			}
			a := &schema.Answers{SchemaVersion: 1, Answers: []schema.Answer{{
				QuestionID: f.e.State.Progress.Pending[0].ID,
				Answer:     "Follow the task only for the obsolete damaged-run field shape. Retain and verify every other requirement in draft.md.",
			}}}
			// Round 2 (PR10-03): a closed question refuses this scoped answer on receipt, so it can
			// never become a waiver; the question stays open and nothing is approved.
			if err := ApplyAnswers(f.e.Run, f.e.State, a, time.Now()); err != nil {
				if len(f.e.State.Progress.Pending) != 1 || f.e.State.Status == run.StatusApproved {
					t.Fatalf("a refused answer changed the run: %v pending=%d status=%s", err, len(f.e.State.Progress.Pending), f.e.State.Status)
				}
				return
			}
			if o := f.execute(t); o.Status == run.StatusApproved {
				v, note := library.Verify(f.e.State.Publish.Path)
				t.Fatalf("a scoped answer suppressed a new %s assessment: status=%s attempts=%d pending=%d verify=%s %s", tc.verdict, o.Status, f.e.State.Counters.Attempts, len(f.e.State.Progress.Pending), v, note)
			}
		})
	}
}
