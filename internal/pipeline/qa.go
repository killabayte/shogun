package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/killabayte/shogun/internal/planning/schema"
	"github.com/killabayte/shogun/internal/run"
)

// MaxQuestionRounds: after two rounds of questions Shogun does not plan on guesses (§7).
const MaxQuestionRounds = 2

// Decision is an answer or an accepted assumption, archived in decisions.json and shown to
// both models as binding.
type Decision struct {
	ID       string `json:"id"`
	Stage    string `json:"stage"`
	Origin   string `json:"origin"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Source   string `json:"source"` // user | answers-file | assumption
	At       string `json:"at"`
}

// Asker asks the user in a terminal. An empty answer accepts the proposed assumption.
type Asker interface {
	Ask(ctx context.Context, qs []run.Pending) (map[string]string, error)
}

func loadDecisions(dir string) ([]Decision, error) {
	b, err := os.ReadFile(filepath.Join(dir, "decisions.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ds []Decision
	return ds, json.Unmarshal(b, &ds)
}

func saveDecisions(r *run.Run, ds []Decision) error {
	b, err := json.MarshalIndent(ds, "", " ")
	if err != nil {
		return err
	}
	return r.WriteArtifact("decisions.json", b)
}

// addQuestions queues model questions under Shogun ids (models number from Q-001 every call).
// Repeats are dropped only when nothing changes: a question the user answered, a non-blocking
// repeat of an assumption, or a repeat of a pending question. A blocking repeat of an assumption
// is asked; a blocking repeat of a pending non-blocking question makes it blocking.
func addQuestions(p *run.Progress, stage, origin string, qs []schema.Question, settled []Decision) {
	answered, assumed := map[string]bool{}, map[string]bool{}
	for _, d := range settled {
		if d.Source == "assumption" {
			assumed[normQuestion(d.Question)] = true
		} else {
			answered[normQuestion(d.Question)] = true
		}
	}
	pending := map[string]int{}
	for i, q := range p.Pending {
		pending[normQuestion(q.Question)] = i
	}
	for _, q := range qs {
		key := normQuestion(q.Question)
		if answered[key] || (assumed[key] && !q.Blocking) {
			continue
		}
		if i, ok := pending[key]; ok {
			if q.Blocking && !p.Pending[i].Blocking {
				p.Pending[i].Blocking, p.Pending[i].Why, p.Pending[i].Impact = true, q.Why, q.Impact
			}
			continue
		}
		pending[key] = len(p.Pending)
		p.NextQuestion++
		p.Pending = append(p.Pending, run.Pending{ID: fmt.Sprintf("Q-%03d", p.NextQuestion), Stage: stage, Origin: origin,
			Question: q.Question, Why: q.Why, Impact: q.Impact, Options: q.Options, ProposedAssumption: q.ProposedAssumption, Blocking: q.Blocking})
	}
}

func normQuestion(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }

func hasBlocking(ps []run.Pending) bool {
	for _, q := range ps {
		if q.Blocking {
			return true
		}
	}
	return false
}

// record turns answers into decisions; a pending question without an answer takes its proposed
// assumption, which is only allowed for non-blocking questions.
func record(ds []Decision, pending []run.Pending, answers map[string]string, source string, now time.Time) ([]Decision, error) {
	ts := now.UTC().Format(time.RFC3339)
	for _, q := range pending {
		a, src := strings.TrimSpace(answers[q.ID]), source
		if a == "" {
			if q.Blocking || q.ProposedAssumption == "" {
				return ds, fmt.Errorf("question %s needs an answer: %s", q.ID, q.Question)
			}
			a, src = q.ProposedAssumption, "assumption"
		}
		if q.Closed {
			// Checked on receipt, before any model call, and stored as the option's full text so both
			// models see what was chosen, not a bare number.
			choice := closedChoice(q.Options, a)
			if choice == "" {
				return ds, fmt.Errorf("question %s takes one of its options, by number or exact text (got %q): %s", q.ID, a, optionList(q.Options))
			}
			a = choice
		}
		ds = append(ds, Decision{ID: q.ID, Stage: q.Stage, Origin: q.Origin, Question: q.Question, Answer: a, Source: src, At: ts})
	}
	return ds, nil
}

// closedChoice maps an answer to the option it selects: its number or its exact text (case and
// spacing ignored). Anything else selects nothing.
func closedChoice(options []string, answer string) string {
	a := normQuestion(answer)
	for i, o := range options {
		if a == fmt.Sprint(i+1) || a == normQuestion(o) {
			return o
		}
	}
	return ""
}

func optionList(options []string) string {
	var b strings.Builder
	for i, o := range options {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%d) %s", i+1, o)
	}
	return b.String()
}

// resolveQuestions settles the pending questions: interactively (at most two rounds), or in
// --auto mode by taking the assumptions of non-blocking ones. A blocking question without a
// terminal, or after two rounds, stops the run with needs_input.
func (e *Engine) resolveQuestions(ctx context.Context) *Outcome {
	p := &e.State.Progress
	if len(p.Pending) == 0 {
		return nil
	}
	ds, err := loadDecisions(e.Run.Dir)
	if err != nil {
		return e.fail("decisions", err)
	}
	if e.Asker == nil || p.QuestionRounds >= MaxQuestionRounds {
		if hasBlocking(p.Pending) || p.QuestionRounds >= MaxQuestionRounds {
			return e.needsInput(fmt.Sprintf("%d question(s) need an answer; see questions.json", len(p.Pending)))
		}
		if ds, err = record(ds, p.Pending, nil, "assumption", e.Now()); err != nil {
			return e.fail("decisions", err)
		}
	} else {
		e.flushActive()
		waitStart := time.Now()
		answers, err := e.Asker.Ask(ctx, p.Pending)
		e.State.Counters.WaitSeconds += time.Since(waitStart).Seconds()
		e.segStart = time.Now() // waiting for the user is not active time
		if err != nil {
			return e.fail("questions", err)
		}
		p.QuestionRounds++
		if ds, err = record(ds, p.Pending, answers, "user", e.Now()); err != nil {
			return e.needsInput(err.Error())
		}
	}
	if err := saveDecisions(e.Run, ds); err != nil {
		return e.fail("decisions", err)
	}
	p.Pending = nil
	return e.checkpoint()
}

// ApplyAnswers is the resume path for a needs_input run: every blocking pending question must be
// answered; unanswered non-blocking ones take their assumption.
func ApplyAnswers(r *run.Run, st *run.State, a *schema.Answers, now time.Time) error {
	p := &st.Progress
	if len(p.Pending) == 0 {
		return errors.New("the run has no pending questions")
	}
	known := map[string]bool{}
	for _, q := range p.Pending {
		known[q.ID] = true
	}
	answers := map[string]string{}
	for _, x := range a.Answers {
		if !known[x.QuestionID] {
			return fmt.Errorf("answer for %s, which is not a pending question", x.QuestionID)
		}
		answers[x.QuestionID] = x.Answer
	}
	ds, err := loadDecisions(r.Dir)
	if err != nil {
		return err
	}
	if ds, err = record(ds, p.Pending, answers, "answers-file", now); err != nil {
		return err
	}
	if err := saveDecisions(r, ds); err != nil {
		return err
	}
	p.Pending, p.QuestionRounds = nil, p.QuestionRounds+1
	st.Status, st.Reason = run.StatusRunning, ""
	return nil
}

// writeQuestions stores the pending questions for `resume --answers`.
func writeQuestions(r *run.Run, ps []run.Pending) error {
	b, err := json.MarshalIndent(map[string]any{"schema_version": 1, "questions": ps}, "", " ")
	if err != nil {
		return err
	}
	return r.WriteArtifact("questions.json", b)
}
