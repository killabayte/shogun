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
	// QuestionsVersion names the question set a report was made with. v3 was the first live run
	// (2026-09-29, below the rule); v4 is the one permitted rewrite: literal phrasing, the task and
	// the item side by side, the item excluded from its context, verification per criterion.
	QuestionsVersion = 4
	// criteriaInline is how many per-criterion checks ride in a step's own request (with its four
	// base questions, within the eight-question rule); more go to one extra request per eight.
	criteriaInline = 4
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
// "a|b" sums the probabilities of several options of one choice (a rule stated before the v4 run:
// in v3 the two adverse role options split 0.48/0.44 and neither crossed the threshold).
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
		{Note: "S-002's tests could not detect wrong totals (the reviewer's major): criterion R-001.C4", Any: []Signal{{"S-002", "bites_R_001_C4", "false"}}},
	}},
	{Name: "102433", Label: "good", Dir: "102433"},
	{Name: "mutA", Label: "defective", Mutation: "mutA-extra-step.json", Expect: []Expect{
		{Note: "an added --csv step the task never asked for", Any: []Signal{{"S-003", "role", "optional_improvement|out_of_scope"}, {"S-003", "adds_capability", "true"}}},
	}},
	{Name: "mutB", Label: "defective", Mutation: "mutB-extra-requirement.json", Expect: []Expect{
		{Note: "an added logging requirement the task never asked for", Any: []Signal{{"R-006", "asked_by", "nothing_asks_it"}}},
	}},
	{Name: "mutC", Label: "defective", Mutation: "mutC-hollow-verification.json", Expect: []Expect{
		{Note: "S-002's verification replaced by 'go build succeeds': no criterion is detected", Any: []Signal{
			{"S-002", "bites_R_001_C1", "false"}, {"S-002", "bites_R_001_C2", "false"}, {"S-002", "bites_R_001_C3", "false"}, {"S-002", "bites_R_002_C1", "false"},
			{"S-002", "bites_R_003_C1", "false"}, {"S-002", "bites_R_003_C2", "false"}, {"S-002", "bites_R_004_C1", "false"}, {"S-002", "bites_R_005_C1", "false"}}},
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
			return task, stepItems(task, plan, decisions, s), nil
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
		items = append(items, stepItems(task, plan, decisions, s)...)
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
			"weakens_task":    jev.Noul("Read `task` and `assumption.answer`. Does `task` say that something must happen or must be present, and `assumption.answer` say that it will not happen, will happen differently, or will happen only in some cases?"),
			"already_decided": jev.Noul("Does `task` or an entry of `decisions` already state the answer to `assumption.question`, and does `assumption.answer` say something different from it?"),
		},
		Adverse: []Adverse{{"weakens_task", "true", SignalThreshold}, {"already_decided", "true", SignalThreshold}},
	}
}

// §3.2
func requirementItem(task string, all []Requirement, decisions []Decision, r Requirement) Item {
	list := []string{} // context only, and without the requirement under test
	for _, x := range all {
		if x.ID != r.ID {
			list = append(list, x.ID+": "+x.Statement)
		}
	}
	var crit []string
	for _, c := range r.Criteria {
		crit = append(crit, c.ID+": "+c.Text)
	}
	if crit == nil {
		crit = []string{}
	}
	return Item{ID: r.ID, Kind: "requirement",
		State: map[string]any{"task": task, "decisions": binding(decisions),
			"requirement":        map[string]any{"id": r.ID, "statement": r.Statement, "type": r.Type, "mandatory": r.Mandatory, "criteria": crit},
			"other_requirements": list},
		Questions: map[string]jev.Question{
			"asked_by": jev.Choice("Compare `task` with `requirement.statement`. `task` and `decisions` are the only things that can ask for a requirement; `other_requirements` are context and cannot. Which is true?", map[string]string{
				"task_states_it":   "a sentence of `task` asks for what `requirement.statement` describes, in these words or equivalent words",
				"needed_for_task":  "`task` does not say it, but what `task` asks for cannot be done or shown without it",
				"decision_asks_it": "an entry of `decisions` asks for it",
				"nothing_asks_it":  "no sentence of `task` and no entry of `decisions` asks for it, and `task` can be done without it",
				"unknown":          "cannot tell from the state"}),
			"narrows_task":   jev.Noul("Compare `task` with `requirement.statement` and `requirement.criteria`. Does `task` name items, cases or fields about the same matter that `requirement.statement` or `requirement.criteria` leave out or exclude?"),
			"criteria_match": jev.Noul("Do `requirement.criteria` check `requirement.statement` itself rather than something else?"),
		},
		Adverse: []Adverse{{"asked_by", "nothing_asks_it", SignalThreshold}, {"narrows_task", "true", SignalThreshold}, {"criteria_match", "false", SignalThreshold}},
	}
}

// §3.3 (v4): one request with the step's four base questions and up to criteriaInline
// per-criterion verification checks; further criteria go to extra requests ("S-NNN#crit…") of
// eight checks each. The per-criterion check replaces the all-criteria verification_bites of v3.
func stepItems(task string, plan *Plan, decisions []Decision, s Step) []Item {
	text := map[string]string{}
	for _, r := range plan.Requirements {
		for _, c := range r.Criteria {
			text[c.ID] = c.Text
		}
	}
	crit := []string{}
	var critIDs []string
	for _, id := range s.CriterionIDs {
		if t, ok := text[id]; ok {
			crit = append(crit, id+": "+t)
			critIDs = append(critIDs, id)
		}
	}
	ver := []string{}
	for _, v := range s.Verification {
		ver = append(ver, v.ID+" ("+v.Method+"): "+v.Expected)
	}
	state := map[string]any{"task": task, "decisions": binding(decisions), "criteria": crit,
		"step": map[string]any{"id": s.ID, "title": s.Title, "objective": s.Objective, "actions": s.Actions, "verification": ver}}
	base := Item{ID: s.ID, Kind: "step", State: state,
		Questions: map[string]jev.Question{
			"role": jev.Choice("Compare `task` with `step`. What is `step` for?", map[string]string{
				"required_by_task":            "what `task` asks for cannot be done without it",
				"preparation_or_verification": "it prepares or proves what `task` asks for",
				"optional_improvement":        "it would be nice, and `task` does not ask for it",
				"out_of_scope":                "it has nothing to do with `task`",
				"unknown":                     "cannot tell from the state"}),
			"adds_capability":  jev.Noul("Do `step.actions` add a capability, option or generality that neither `task` nor `criteria` ask for?"),
			"oversized":        jev.Noul("Do `step.actions` do more than `step.objective` and `criteria` need?"),
			"contradicts_task": jev.Noul("Compare `task` with `step.actions`. Does `task` say that something must happen or must be present, and `step.actions` make it not happen or leave it out? Or does `task` say something must not happen, and `step.actions` do it?"),
		},
		Adverse: []Adverse{{"role", "optional_improvement|out_of_scope", SignalThreshold},
			{"adds_capability", "true", SignalThreshold}, {"oversized", "true", SignalThreshold}, {"contradicts_task", "true", SignalThreshold}},
	}
	check := func(id string) (string, jev.Question) {
		name := "bites_" + strings.NewReplacer("-", "_", ".", "_").Replace(id)
		return name, jev.Noul("Read `step.verification` and the criterion " + id + " in `criteria`. If the step's result violated " + id + ", would at least one of `step.verification`, as written, fail? Existing tests count when they assert this criterion.")
	}
	items := []Item{base}
	for i, id := range critIDs {
		name, q := check(id)
		target := &items[0]
		if i >= criteriaInline {
			n := 1 + (i-criteriaInline)/8
			extraID := fmt.Sprintf("%s#crit%d", s.ID, n)
			if items[len(items)-1].ID != extraID {
				items = append(items, Item{ID: extraID, Kind: "step-criteria", State: state, Questions: map[string]jev.Question{}})
			}
			target = &items[len(items)-1]
		}
		target.Questions[name] = q
		target.Adverse = append(target.Adverse, Adverse{name, "false", SignalThreshold})
	}
	return items
}

// Hit reports whether an answer meets an adverse spec, and the probability compared: the sum over
// the outcomes named in Outcome ("a|b").
func Hit(a jev.Answer, adv Adverse) (bool, float64) {
	p := 0.0
	for _, o := range strings.Split(adv.Outcome, "|") {
		p += a.P(o)
	}
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
