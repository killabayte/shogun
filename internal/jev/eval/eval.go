// Package eval is the J0 evaluation of Jev alone (docs/plans/shogun-jev.md §7): frozen plans with
// known defects and known-good plans, turned into per-item requests with the design's questions,
// and expectations written before any score was seen. It changes nothing in the planning pipeline.
package eval

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/killabayte/shogun/internal/jev"
)

//go:embed testdata
var testdata embed.FS

// Thresholds of the design (§3): provisional until this evaluation sets them.
const (
	SignalThreshold = 0.80 // adverse P at or above → a signal
	TrimThreshold   = 0.90 // reported for information only (§2.1)
)

// Plan is the subset of a planner document the evaluation reads.
type Plan struct {
	Requirements []Requirement `json:"requirements"`
	Steps        []Step        `json:"steps"`
	Questions    []struct {
		ID                 string `json:"id"`
		Question           string `json:"question"`
		ProposedAssumption string `json:"proposed_assumption"`
	} `json:"questions"`
}

type Requirement struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
	Type      string `json:"type"`
	Mandatory bool   `json:"mandatory"`
	Criteria  []struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	} `json:"criteria"`
}

type Step struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Objective    string   `json:"objective"`
	CriterionIDs []string `json:"criterion_ids"`
	Actions      []string `json:"actions"`
	Verification []struct {
		ID       string `json:"id"`
		Method   string `json:"method"`
		Expected string `json:"expected"`
	} `json:"verification"`
}

// Decision is one entry of decisions.json; only Source "assumption" entries are assumptions.
type Decision struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Source   string `json:"source"`
}

// Adverse names the outcome of one question that counts against the item: it fires when
// P(outcome) ≥ Threshold. For a noul the outcome is "true" or "false" (P(false) = 1 − P(true)), so
// "criteria do not match" is the outcome "false" of criteria_match at 0.80, i.e. P(true) ≤ 0.20.
type Adverse struct {
	Question  string
	Outcome   string
	Threshold float64
}

// Item is one request: an assumption, a requirement or a step with its state and questions.
type Item struct {
	ID        string
	Kind      string // assumption | requirement | step
	State     any
	Questions map[string]jev.Question
	Adverse   []Adverse
}

// Expect is one labelled expectation: the case is defective and this signal should fire. Any of
// the listed alternatives counts as caught.
type Expect struct {
	Note string
	Any  []Signal
}

// Signal identifies an adverse outcome on an item.
type Signal struct {
	Item, Question, Outcome string
}

// Case is one frozen artifact with its label and expectations.
type Case struct {
	Name     string
	Label    string // good | defective
	Dir      string // testdata/<dir>, or "" for a mutation
	Mutation string // testdata/<file>.json for a single-item case
	Expect   []Expect
}

// Cases are the §7 evaluation set. Labels and expectations were written before the first run.
var Cases = []Case{
	{Name: "5d31", Label: "good", Dir: "5d31"},
	{Name: "1b4e", Label: "defective", Dir: "1b4e", Expect: []Expect{
		{Note: "Q-002 keeps the stderr note under --json, against 'one JSON document on stdout'", Any: []Signal{{"A-002", "weakens_task", "true"}}},
		{Note: "S-001's no-runs action contradicts the task", Any: []Signal{{"S-001", "contradicts_task", "true"}}},
	}},
	{Name: "6be3", Label: "defective", Dir: "6be3", Expect: []Expect{
		{Note: "R-002 drops the fields of a damaged run that the task lists for every run", Any: []Signal{{"R-002", "narrows_task", "true"}}},
		{Note: "S-001's damaged-run action carries only id, status, damaged", Any: []Signal{{"S-001", "contradicts_task", "true"}}},
	}},
	{Name: "a592r1", Label: "defective", Dir: "a592r1", Expect: []Expect{
		{Note: "damaged-run object shape (the reviewer's major): R-002 narrows or S-001 contradicts", Any: []Signal{{"R-002", "narrows_task", "true"}, {"S-001", "contradicts_task", "true"}}},
	}},
	{Name: "08fdr1", Label: "defective", Dir: "08fdr1", Expect: []Expect{
		{Note: "S-002's tests could not detect wrong totals (the reviewer's major)", Any: []Signal{{"S-002", "verification_bites", "false"}}},
	}},
	{Name: "102433", Label: "good", Dir: "102433"},
	{Name: "mutA", Label: "defective", Mutation: "mutA-extra-step.json", Expect: []Expect{
		{Note: "an added --csv step the task never asked for", Any: []Signal{{"S-003", "role", "optional_improvement"}, {"S-003", "role", "out_of_scope"}, {"S-003", "adds_capability", "true"}}},
	}},
	{Name: "mutB", Label: "defective", Mutation: "mutB-extra-requirement.json", Expect: []Expect{
		{Note: "an added logging requirement the task never asked for", Any: []Signal{{"R-006", "origin", "not_asked"}}},
	}},
	{Name: "mutC", Label: "defective", Mutation: "mutC-hollow-verification.json", Expect: []Expect{
		{Note: "S-002's verification replaced by 'go build succeeds'", Any: []Signal{{"S-002", "verification_bites", "false"}}},
	}},
}

// Load reads a case's task, plan and decisions from the embedded testdata and builds its items. A
// mutation case borrows 5d31's task and requirements and yields exactly one item.
func Load(c Case) (task string, items []Item, err error) {
	if c.Mutation != "" {
		task, plan, decisions, err := readCase("5d31")
		if err != nil {
			return "", nil, err
		}
		var m struct {
			Kind string          `json:"kind"`
			Item json.RawMessage `json:"item"`
		}
		b, err := fs.ReadFile(testdata, path.Join("testdata", c.Mutation))
		if err != nil {
			return "", nil, err
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return "", nil, fmt.Errorf("%s: %w", c.Mutation, err)
		}
		switch m.Kind {
		case "step":
			var s Step
			if err := json.Unmarshal(m.Item, &s); err != nil {
				return "", nil, err
			}
			return task, []Item{stepItem(task, plan, decisions, s)}, nil
		case "requirement":
			var r Requirement
			if err := json.Unmarshal(m.Item, &r); err != nil {
				return "", nil, err
			}
			all := append(append([]Requirement{}, plan.Requirements...), r)
			return task, []Item{requirementItem(task, all, decisions, r)}, nil
		}
		return "", nil, fmt.Errorf("%s: unknown mutation kind %q", c.Mutation, m.Kind)
	}
	task, plan, decisions, err := readCase(c.Dir)
	if err != nil {
		return "", nil, err
	}
	return task, Items(task, plan, decisions), nil
}

func readCase(dir string) (string, *Plan, []Decision, error) {
	read := func(name string) ([]byte, error) { return fs.ReadFile(testdata, path.Join("testdata", dir, name)) }
	tb, err := read("task.md")
	if err != nil {
		return "", nil, nil, err
	}
	pb, err := read("plan.json")
	if err != nil {
		return "", nil, nil, err
	}
	var plan Plan
	if err := json.Unmarshal(pb, &plan); err != nil {
		return "", nil, nil, fmt.Errorf("%s/plan.json: %w", dir, err)
	}
	var decisions []Decision
	if db, err := read("decisions.json"); err == nil {
		if err := json.Unmarshal(db, &decisions); err != nil {
			return "", nil, nil, fmt.Errorf("%s/decisions.json: %w", dir, err)
		}
	}
	return string(tb), &plan, decisions, nil
}

// Items builds the requests of one plan in the design's order: assumptions, steps, requirements.
func Items(task string, plan *Plan, decisions []Decision) []Item {
	var items []Item
	n := 0
	// The latest decision on a question is the one in force: an assumption only while no binding
	// answer (user or answers-file) has replaced it.
	latest := map[string]Decision{}
	var order []string
	for _, d := range decisions {
		k := norm(d.Question)
		if _, ok := latest[k]; !ok {
			order = append(order, k)
		}
		latest[k] = d
	}
	for _, k := range order {
		if d := latest[k]; d.Source == "assumption" {
			n++
			items = append(items, assumptionItem(task, decisions, fmt.Sprintf("A-%03d", n), d.Question, d.Answer))
		}
	}
	for _, q := range plan.Questions { // a planner question with no decision at all: its proposal
		if _, decided := latest[norm(q.Question)]; !decided && q.ProposedAssumption != "" {
			n++
			items = append(items, assumptionItem(task, decisions, fmt.Sprintf("A-%03d", n), q.Question, q.ProposedAssumption))
		}
	}
	for _, s := range plan.Steps {
		items = append(items, stepItem(task, plan, decisions, s))
	}
	for _, r := range plan.Requirements {
		items = append(items, requirementItem(task, plan.Requirements, decisions, r))
	}
	return items
}

func norm(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }

func binding(decisions []Decision) []string {
	var out []string
	for _, d := range decisions {
		if d.Source != "assumption" {
			out = append(out, d.Question+" → "+d.Answer)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// §3.1
func assumptionItem(task string, decisions []Decision, id, question, answer string) Item {
	return Item{ID: id, Kind: "assumption",
		State: map[string]any{"task": task, "decisions": binding(decisions), "assumption": map[string]string{"question": question, "answer": answer}},
		Questions: map[string]jev.Question{
			"weakens_task":    jev.Noul("Does `assumption` drop, weaken or make an exception to something `task` explicitly requires?"),
			"already_decided": jev.Noul("Is the matter of `assumption` already settled by `task` or by an entry of `decisions` in a way that `assumption` does not follow? An assumption that restates what is already decided is not such a case."),
		},
		Adverse: []Adverse{{"weakens_task", "true", SignalThreshold}, {"already_decided", "true", SignalThreshold}},
	}
}

// §3.2
func requirementItem(task string, all []Requirement, decisions []Decision, r Requirement) Item {
	var list []string
	for _, x := range all {
		list = append(list, x.ID+": "+x.Statement)
	}
	var crit []string
	for _, c := range r.Criteria {
		crit = append(crit, c.ID+": "+c.Text)
	}
	if crit == nil {
		crit = []string{}
	}
	return Item{ID: r.ID, Kind: "requirement",
		State: map[string]any{"task": task, "decisions": binding(decisions), "all_requirements": list,
			"requirement": map[string]any{"id": r.ID, "statement": r.Statement, "type": r.Type, "mandatory": r.Mandatory, "criteria": crit}},
		Questions: map[string]jev.Question{
			"origin": jev.Choice("Where does `requirement` come from?", map[string]string{
				"task_explicit":              "the task states it",
				"task_necessary_consequence": "the task does not state it, but it cannot be done without this",
				"decision":                   "a binding entry of `decisions` asks for it",
				"not_asked":                  "neither the task nor a decision asks for it",
				"unknown":                    "cannot tell from the state"}),
			"narrows_task":   jev.Noul("Does `requirement` drop or weaken part of what `task` requires about the same matter? `all_requirements` may cover other matters; judge only this requirement's own matter."),
			"criteria_match": jev.Noul("Do `requirement.criteria` check `requirement.statement` itself rather than something else?"),
		},
		Adverse: []Adverse{{"origin", "not_asked", SignalThreshold}, {"narrows_task", "true", SignalThreshold}, {"criteria_match", "false", SignalThreshold}},
	}
}

// §3.3
func stepItem(task string, plan *Plan, decisions []Decision, s Step) Item {
	text := map[string]string{}
	for _, r := range plan.Requirements {
		for _, c := range r.Criteria {
			text[c.ID] = c.Text
		}
	}
	crit := []string{}
	for _, id := range s.CriterionIDs {
		if t, ok := text[id]; ok {
			crit = append(crit, id+": "+t)
		}
	}
	var ver []string
	for _, v := range s.Verification {
		ver = append(ver, v.ID+" ("+v.Method+"): "+v.Expected)
	}
	if ver == nil {
		ver = []string{}
	}
	return Item{ID: s.ID, Kind: "step",
		State: map[string]any{"task": task, "decisions": binding(decisions), "criteria": crit,
			"step": map[string]any{"id": s.ID, "title": s.Title, "objective": s.Objective, "actions": s.Actions, "verification": ver}},
		Questions: map[string]jev.Question{
			"role": jev.Choice("What is `step` for?", map[string]string{
				"required_by_task":            "the task cannot be done without it",
				"preparation_or_verification": "needed to do or to prove the task",
				"optional_improvement":        "would be nice; the task does not ask for it",
				"out_of_scope":                "unrelated to the task",
				"unknown":                     "cannot tell from the state"}),
			"adds_capability":    jev.Noul("Do `step.actions` add a capability, option or generality that neither `task` nor `criteria` ask for?"),
			"oversized":          jev.Noul("Do `step.actions` do more than `step.objective` and `criteria` need?"),
			"verification_bites": jev.Noul("Would `step.verification`, as written, detect a violation of every one of `criteria` if the step's result broke that criterion? Existing tests count when they assert the criterion. Answer no if at least one listed criterion could be violated without any listed verification failing."),
			"contradicts_task":   jev.Noul("Does any of `step.actions` do the opposite of something `task` explicitly requires?"),
		},
		Adverse: []Adverse{{"role", "optional_improvement", SignalThreshold}, {"role", "out_of_scope", SignalThreshold},
			{"adds_capability", "true", SignalThreshold}, {"oversized", "true", SignalThreshold},
			{"verification_bites", "false", SignalThreshold}, {"contradicts_task", "true", SignalThreshold}},
	}
}

// Hit reports whether an answer meets an adverse spec, and the probability compared.
func Hit(a jev.Answer, adv Adverse) (bool, float64) {
	p := a.P(adv.Outcome)
	return p >= adv.Threshold, p
}

// SortedIDs lists item ids in request order (a helper for tables).
func SortedIDs(items []Item) []string {
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	sort.Strings(ids)
	return ids
}
