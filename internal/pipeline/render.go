package pipeline

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/killabayte/shogun/internal/library"
	"github.com/killabayte/shogun/internal/planning/schema"
)

// frontmatter is rendered in this field order (§11). status, tags and updated are the mutable
// allowlist; everything else is bound to the approval.
type frontmatter struct {
	Title    string   `yaml:"title"`
	PlanID   string   `yaml:"plan_id"`
	Revision int      `yaml:"revision"`
	Created  string   `yaml:"created"`
	Updated  string   `yaml:"updated"`
	Status   string   `yaml:"status"`
	RunID    string   `yaml:"run_id"`
	Project  string   `yaml:"project"`
	Repos    []string `yaml:"repos"`
	Tags     []string `yaml:"tags"`
	Planner  string   `yaml:"planner"`
	Reviewer string   `yaml:"reviewer"`
}

// planData is everything the renderer reads; it comes only from approved artifacts.
type planData struct {
	research  *schema.Research
	outline   *schema.Outline
	steps     []schema.Step // accepted, in dependency order
	decisions []Decision
}

// loadPlanData reads the approved research, outline and accepted steps.
func (e *Engine) loadPlanData() (*planData, error) {
	r, err := e.approvedResearch()
	if err != nil || r == nil {
		return nil, fmt.Errorf("no approved research: %v", err)
	}
	o, err := e.approvedOutline()
	if err != nil {
		return nil, err
	}
	d := &planData{research: r, outline: o}
	for _, s := range order(o) {
		n := e.State.Progress.Accepted[s.ID]
		if n == 0 {
			return nil, fmt.Errorf("step %s is not accepted", s.ID)
		}
		st, err := schemaStep(mustRead(filepath.Join(e.Run.Dir, "steps", s.ID, fmt.Sprintf("%d.json", n))))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.ID, err)
		}
		d.steps = append(d.steps, *st)
	}
	if d.decisions, err = loadDecisions(e.Run.Dir); err != nil {
		return nil, err
	}
	return d, nil
}

// render assembles the candidate plan deterministically (§10): frontmatter, exactly one pair of
// markers around the approved body, then the execution log outside the approval area.
func (e *Engine) render(d *planData) ([]byte, error) {
	created := e.State.CreatedAt
	if t, err := time.Parse(time.RFC3339, created); err == nil {
		created = t.UTC().Format("2006-01-02")
	}
	var repos []string
	for _, r := range e.Manifest.Repos {
		repos = append(repos, filepath.Base(r.Root))
	}
	fm := frontmatter{Title: e.title(), PlanID: e.State.RunID, Revision: e.State.Generation, Created: created, Updated: created,
		Status: "planned", RunID: e.State.RunID, Project: e.Project, Repos: repos, Tags: []string{"shogun", "plan"},
		Planner: e.Cfg.Planner.String(), Reviewer: e.Cfg.Reviewer.String()}
	head, err := yaml.Marshal(fm)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString("---\n")
	b.Write(head)
	b.WriteString("---\n\n")
	b.WriteString(library.MarkerBegin + "\n")
	b.WriteString(e.body(d))
	b.WriteString(library.MarkerEnd + "\n\n")
	b.WriteString(executionLog(d))
	return b.Bytes(), nil
}

// title is the task's first line, trimmed to a readable length.
func (e *Engine) title() string {
	t, _, _ := strings.Cut(strings.TrimSpace(e.Task), "\n")
	t = strings.TrimSpace(strings.TrimLeft(t, "# "))
	if r := []rune(t); len(r) > 100 {
		t = string(r[:97]) + "…"
	}
	return t
}

func (e *Engine) body(d *planData) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	reqByType := map[string][]schema.Requirement{}
	critText := map[string]string{}
	for _, r := range d.research.Requirements {
		reqByType[r.Type] = append(reqByType[r.Type], r)
		for _, c := range r.Criteria {
			critText[c.ID] = c.Text
		}
	}

	w("\n# %s\n\n", clean(e.title()))
	w("## Goal and success criteria\n\n")
	w("Task, verbatim:\n\n%s\n\n", quote(e.Task))
	for _, r := range reqByType["functional"] {
		w("- **%s** %s\n", r.ID, clean(r.Statement))
		for _, c := range r.Criteria {
			w("  - %s: %s\n", c.ID, clean(c.Text))
		}
	}
	w("\n## Scope and non-goals\n\n")
	if len(reqByType["constraint"])+len(reqByType["non_goal"]) == 0 {
		w("No constraints or non-goals beyond the requirements.\n")
	}
	for _, kind := range []string{"constraint", "non_goal"} {
		for _, r := range reqByType[kind] {
			w("- **%s** (%s) %s\n", r.ID, strings.ReplaceAll(kind, "_", "-"), clean(r.Statement))
			for _, c := range r.Criteria {
				w("  - %s: %s\n", c.ID, clean(c.Text))
			}
		}
	}

	w("\n## Inputs and versions\n\n")
	for _, r := range e.Manifest.Repos {
		state := "clean"
		if r.DiffSHA256 != "" || r.UntrackedSHA256 != "" {
			state = "with local changes (fingerprint " + short(r.Fingerprint) + ")"
		}
		if r.IsGit {
			w("- %s: `%s` at `%s`, %s\n", r.ID, filepath.Base(r.Root), short(r.Head), state)
		} else {
			w("- %s: `%s` (not a git repository, fingerprint %s)\n", r.ID, filepath.Base(r.Root), short(r.Fingerprint))
		}
	}
	for _, s := range e.sources() {
		w("- %s: `%s`, sha256 `%s`\n", s.ID, clean(s.Origin), short(s.SHA256))
	}
	for _, s := range e.webRecords() {
		if s.Status == "ok" {
			w("- %s: <%s>, archived sha256 `%s`\n", s.ID, s.Origin, short(s.SHA256))
		}
	}

	w("\n## Requirements\n\n| ID | Type | Mandatory | Statement | Criteria | Sources |\n|---|---|---|---|---|---|\n")
	for _, r := range d.research.Requirements {
		var cs []string
		for _, c := range r.Criteria {
			cs = append(cs, c.ID+": "+c.Text)
		}
		w("| %s | %s | %v | %s | %s | %s |\n", r.ID, r.Type, r.Mandatory, cell(r.Statement), cell(strings.Join(cs, "; ")), cell(strings.Join(r.SourceIDs, ", ")))
	}

	w("\n## Decisions and assumptions\n\n")
	if len(d.decisions) == 0 {
		w("None.\n")
	}
	for _, x := range d.decisions {
		w("- %s (%s): %s → %s\n", x.ID, x.Source, clean(x.Question), clean(x.Answer))
	}

	w("\n## Context\n\n")
	for _, f := range d.research.Facts {
		w("- %s `%s:%s` (%s): %s", f.ID, f.SourceID, clean(f.Location), f.Kind, clean(f.Text))
		if f.Quote != "" {
			w(" — “%s”", clean(f.Quote))
		}
		w("\n")
	}

	w("\n## Approach\n\n%s\n", clean(d.outline.Approach.Summary))
	for _, a := range d.outline.Approach.Alternatives {
		w("- Not chosen: %s — %s\n", clean(a.Name), clean(a.WhyNot))
	}

	w("\n## Steps\n")
	for _, s := range d.steps {
		w("\n### %s — %s\n\n", s.ID, clean(s.Title))
		w("- Objective: %s\n", clean(s.Objective))
		w("- Depends on: %s\n", orNone(s.DependsOn))
		w("- Requirements: %s; criteria: %s\n", orNone(s.RequirementIDs), orNone(s.CriterionIDs))
		w("\nTargets:\n\n")
		for _, t := range s.Targets {
			w("- %s `%s` (%s)\n", t.RepoID, clean(t.Path), t.Operation)
		}
		w("\nActions:\n\n")
		for i, a := range s.Actions {
			w("%d. %s\n", i+1, clean(a))
		}
		w("\nVerification:\n\n")
		for _, v := range s.Verification {
			w("- %s (%s, %s): %s\n", v.ID, v.Method, v.RepoID, clean(v.Expected))
		}
		if len(s.Risks) > 0 {
			w("\nRisks:\n\n")
			for _, r := range s.Risks {
				w("- %s — mitigation: %s\n", clean(r.Risk), clean(r.Mitigation))
			}
		}
		w("\nRollback: %s\n", clean(s.RollbackOrWhyNotApplicable))
	}

	w("\n## End-to-end verification\n\n")
	if len(d.outline.FinalVerificationCriterionIDs) == 0 {
		w("Every criterion is verified inside its step (see the traceability table).\n")
	}
	for _, c := range d.outline.FinalVerificationCriterionIDs {
		w("- %s: %s\n", c, clean(critText[c]))
	}

	w("\n## Traceability\n\n| Requirement | Criterion | Steps | Verification |\n|---|---|---|---|\n")
	for _, r := range d.research.Requirements {
		for _, c := range r.Criteria {
			var steps, verifs []string
			for _, s := range d.steps {
				if contains(s.CriterionIDs, c.ID) {
					steps = append(steps, s.ID)
					for _, v := range s.Verification {
						verifs = append(verifs, s.ID+"/"+v.ID)
					}
				}
			}
			if contains(d.outline.FinalVerificationCriterionIDs, c.ID) {
				steps = append(steps, "end-to-end")
			}
			w("| %s | %s | %s | %s |\n", r.ID, c.ID, orNone(steps), orNone(verifs))
		}
	}

	w("\n## Review history\n\n")
	p := &e.State.Progress
	var keys []string
	for k := range p.Rounds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n := 0
		for _, f := range p.Ledger {
			if f.Stage == k {
				n++
			}
		}
		w("- %s: %d review round(s), %d finding(s)\n", k, p.Rounds[k], n)
	}
	w("\nPlanner %s, reviewer %s. The full call and review history stays in the run directory.\n\n", e.Cfg.Planner, e.Cfg.Reviewer)
	return b.String()
}

// executionLog is the unapproved tail the executor edits (§11).
func executionLog(d *planData) string {
	var b strings.Builder
	b.WriteString("## Execution log\n\n")
	b.WriteString("This section is outside the approved area and is maintained by whoever executes the plan. ")
	b.WriteString("`shogun verify` checks only that the body between the markers and the immutable frontmatter still match the approval receipt; ")
	b.WriteString("it does not check that the work was done, that the repositories are current, or that the solution is correct. ")
	b.WriteString("Statuses: todo, in_progress, blocked, done (with a link to a commit, PR or test report), skipped (with a reason). ")
	b.WriteString("A change of scope needs a new plan revision, not a note.\n\n")
	b.WriteString("| Step | Status | Date / executor | Evidence / deviation |\n|---|---|---|---|\n")
	for _, s := range d.steps {
		fmt.Fprintf(&b, "| %s | todo | — | — |\n", s.ID)
	}
	return b.String()
}

func schemaStep(b []byte) (*schema.Step, error) {
	var s schema.Step
	return &s, jsonUnmarshalStrict(b, &s)
}

// clean keeps model text on one logical line of Markdown and makes the reserved markers inert.
func clean(s string) string {
	s = strings.ReplaceAll(s, "<!-- shogun:plan:", "<!-- shogun-plan:")
	return strings.TrimSpace(s)
}

func cell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(clean(s), "|", `\|`), "\n", " ")
}

func quote(s string) string {
	lines := strings.Split(strings.TrimRight(clean(s), "\n"), "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}
