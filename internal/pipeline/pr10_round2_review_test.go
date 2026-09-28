package pipeline

import (
	"strings"
	"testing"
	"time"

	"github.com/killabayte/shogun/internal/planning/schema"
	"github.com/killabayte/shogun/internal/run"
)

// The source-role question has a closed set of answers. Re-prompting must not
// spend another model pair just to discover an invalid answer locally.
func TestPR10Round2ReviewInteractiveInvalidChoiceDoesNotSpend(t *testing.T) {
	f := fastWithContradictedInput(t)
	f.planner.replies = append(f.planner.replies, f.planner.replies[0])
	f.reviewer.replies = append(f.reviewer.replies, f.reviewer.replies[0])
	f.e.Asker = &answerAll{} // "yes" is nonempty, but neither of the offered choices.
	o := f.execute(t)
	if o.Status != run.StatusNeedsInput || f.e.State.Counters.Attempts != 2 {
		t.Fatalf("invalid interactive answer spent models: status=%s attempts=%d, want needs_input with 2 attempts\n%s", o.Status, f.e.State.Counters.Attempts, f.log.String())
	}
}

// A number is a documented valid answer in answers.json. Its meaning must reach
// both models, just as when terminalAsker expands the selected option.
func TestPR10Round2ReviewNumericAuthoritativeChoiceReachesModels(t *testing.T) {
	f := fastWithContradictedInput(t)
	if o := f.execute(t); o.Status != run.StatusNeedsInput || len(f.e.State.Progress.Pending) != 1 {
		t.Fatalf("expected a source question: %+v", o)
	}
	a := &schema.Answers{SchemaVersion: 1, Answers: []schema.Answer{{
		QuestionID: f.e.State.Progress.Pending[0].ID,
		Answer:     "2",
	}}}
	if err := ApplyAnswers(f.e.Run, f.e.State, a, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"planner", "reviewer"} {
		prompt, err := f.e.prompt(StagePlan, role, 2)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(prompt, "authoritative") {
			t.Errorf("%s gets only a numeric answer without the selected authoritative role or its option text\n%s", role, prompt)
		}
	}
}
