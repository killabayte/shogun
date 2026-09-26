package pipeline

import (
	"strings"
	"testing"

	"github.com/killabayte/shogun/internal/provider"
	"github.com/killabayte/shogun/internal/run"
)

// A finding specifically about the later step must survive a split without
// blocking acceptance of its earlier dependency. The later step cannot be
// edited in the earlier step's exact-batch response.
func TestP4Round2SplitKeepsStepSpecificFindingWithItsStep(t *testing.T) {
	_, _, steps := chain(2)
	large := withDeps(stepBatch(steps...))
	large["steps"].([]any)[0].(map[string]any)["actions"] = []string{strings.Repeat("action ", 20000)}
	corrected := withDeps(stepBatch(steps[1]))
	corrected["steps"].([]any)[0].(map[string]any)["verification"].([]any)[0].(map[string]any)["expected"] = "prints the version and exits 0"
	f, _ := newChain(t, 2,
		[]reply{batch(steps...), fixed(large), batch(steps[0]), fixed(corrected)},
		[]reply{
			fixed(stepReview("revise", []map[string]any{finding("S-002", "major", "S-002's verification needs an explicit expected exit status")}, steps...)),
			nil,
			func(provider.Request) (any, error) {
				d := stepReview("approve", nil, steps[1])
				d["dispositions"] = []any{disposition("F-001", "resolved")}
				return d, nil
			},
		})
	f.e.Cfg.DetailBatch, f.e.Cfg.MaxContextTokens = 2, 8192
	f.reviewer.replies[3] = func(provider.Request) (any, error) {
		// Bound a wrongly blocked child so the regression does not need an
		// unbounded stream of identical planner/reviewer replies.
		f.e.Cfg.ReviewRounds = 1
		d := stepReview("approve", nil, steps[0])
		d["dispositions"] = []any{disposition("F-001", "retained")}
		return d, nil
	}
	o := f.execute(t)
	if f.e.State.Progress.BatchSize != 1 {
		t.Fatalf("control batch did not split: %+v\n%s", o, f.log.String())
	}
	if o.Status != run.StatusPaused || f.e.State.Cursor.Stage != StageIntegration || len(openFindings(&f.e.State.Progress)) != 0 {
		t.Fatalf("S-002 finding blocked its predecessor after split: outcome=%+v cursor=%+v accepted=%v open=%v\n%s",
			o, f.e.State.Cursor, f.e.State.Progress.Accepted, openFindings(&f.e.State.Progress), f.log.String())
	}
}
