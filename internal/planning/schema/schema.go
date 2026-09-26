// Package schema holds the JSON Schemas that form the contract between Shogun and the models,
// the matching Go types, and the semantic checks (ids, references, cycles, coverage) that
// JSON Schema cannot express.
package schema

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed *.schema.json
var files embed.FS

// Kind names a contract document.
type Kind string

const (
	KindRequirements Kind = "requirements"
	KindResearch     Kind = "research"
	KindOutline      Kind = "outline"
	KindStep         Kind = "step"
	KindReview       Kind = "review"
	KindAnswers      Kind = "answers"
)

// Version is the schema_version every document must carry.
const Version = 1

var (
	compileOnce sync.Once
	compiled    map[Kind]*jsonschema.Schema
	compileErr  error
)

func compile() {
	c := jsonschema.NewCompiler()
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		compileErr = err
		return
	}
	for _, e := range entries {
		data, err := files.ReadFile(e.Name())
		if err != nil {
			compileErr = err
			return
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			compileErr = fmt.Errorf("%s: %w", e.Name(), err)
			return
		}
		if err := c.AddResource(e.Name(), doc); err != nil {
			compileErr = fmt.Errorf("%s: %w", e.Name(), err)
			return
		}
	}
	compiled = map[Kind]*jsonschema.Schema{}
	for _, k := range []Kind{KindRequirements, KindResearch, KindOutline, KindStep, KindReview, KindAnswers} {
		s, err := c.Compile(string(k) + ".schema.json")
		if err != nil {
			compileErr = fmt.Errorf("compile %s: %w", k, err)
			return
		}
		compiled[k] = s
	}
}

// Raw returns the embedded schema text for a kind (to pass to a CLI as --json-schema / --output-schema).
func Raw(k Kind) ([]byte, error) { return files.ReadFile(string(k) + ".schema.json") }

// Validate checks data against the schema for kind. The error lists every violation.
func Validate(k Kind, data []byte) error {
	compileOnce.Do(compile)
	if compileErr != nil {
		return compileErr
	}
	s, ok := compiled[k]
	if !ok {
		return fmt.Errorf("unknown schema kind %q", k)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%s: invalid JSON: %w", k, err)
	}
	if err := s.Validate(inst); err != nil {
		return fmt.Errorf("%s: %w", k, err)
	}
	return nil
}

// ---- Go types (mirror the schemas) ----

type Criterion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type Requirement struct {
	ID        string      `json:"id"`
	Statement string      `json:"statement"`
	SourceIDs []string    `json:"source_ids"`
	Type      string      `json:"type"`
	Mandatory bool        `json:"mandatory"`
	Criteria  []Criterion `json:"criteria"`
}

type Requirements struct {
	SchemaVersion int           `json:"schema_version"`
	Requirements  []Requirement `json:"requirements"`
}

type Question struct {
	ID                 string   `json:"id"`
	Question           string   `json:"question"`
	Why                string   `json:"why"`
	Impact             string   `json:"impact"`
	Options            []string `json:"options"`
	ProposedAssumption string   `json:"proposed_assumption"`
	Blocking           bool     `json:"blocking"`
}

type ResponseToFinding struct {
	FindingID string `json:"finding_id"`
	Action    string `json:"action"`
	Note      string `json:"note"`
}

type RequestedChange struct {
	TargetID string `json:"target_id"`
	Reason   string `json:"reason"`
}

type Fact struct {
	ID       string `json:"id"`
	SourceID string `json:"source_id"`
	Location string `json:"location"`
	Quote    string `json:"quote"`
	Kind     string `json:"kind"`
	Text     string `json:"text"`
}

type SourceCoverage struct {
	SourceID  string `json:"source_id"`
	Studied   string `json:"studied"`
	Relevance string `json:"relevance"`
	Notes     string `json:"notes"`
}

type WebSource struct {
	URL        string   `json:"url"`
	Role       string   `json:"role"`
	RelatedIDs []string `json:"related_ids"`
}

type Research struct {
	SchemaVersion       int                 `json:"schema_version"`
	Facts               []Fact              `json:"facts"`
	Requirements        []Requirement       `json:"requirements"`
	SourceCoverage      []SourceCoverage    `json:"source_coverage"`
	Questions           []Question          `json:"questions"`
	WebSources          []WebSource         `json:"web_sources"`
	RequestedChanges    []RequestedChange   `json:"requested_changes"`
	ResponsesToFindings []ResponseToFinding `json:"responses_to_findings"`
}

type OutlineStep struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Objective    string   `json:"objective"`
	Deliverable  string   `json:"deliverable"`
	DependsOn    []string `json:"depends_on"`
	CriterionIDs []string `json:"criterion_ids"`
}

type Outline struct {
	SchemaVersion int `json:"schema_version"`
	Approach      struct {
		Summary      string `json:"summary"`
		Alternatives []struct {
			Name   string `json:"name"`
			WhyNot string `json:"why_not"`
		} `json:"alternatives"`
	} `json:"approach"`
	Steps                         []OutlineStep       `json:"steps"`
	FinalVerificationCriterionIDs []string            `json:"final_verification_criterion_ids"`
	Questions                     []Question          `json:"questions"`
	RequestedChanges              []RequestedChange   `json:"requested_changes"`
	ResponsesToFindings           []ResponseToFinding `json:"responses_to_findings"`
}

type Target struct {
	RepoID    string `json:"repo_id"`
	Path      string `json:"path"`
	Operation string `json:"operation"`
}

type Verification struct {
	ID       string `json:"id"`
	RepoID   string `json:"repo_id"`
	Method   string `json:"method"`
	Expected string `json:"expected"`
}

type Risk struct {
	Risk       string `json:"risk"`
	Mitigation string `json:"mitigation"`
}

type Step struct {
	ID                         string         `json:"id"`
	Title                      string         `json:"title"`
	Objective                  string         `json:"objective"`
	RequirementIDs             []string       `json:"requirement_ids"`
	CriterionIDs               []string       `json:"criterion_ids"`
	DependsOn                  []string       `json:"depends_on"`
	Targets                    []Target       `json:"targets"`
	Actions                    []string       `json:"actions"`
	Verification               []Verification `json:"verification"`
	Risks                      []Risk         `json:"risks"`
	RollbackOrWhyNotApplicable string         `json:"rollback_or_why_not_applicable"`
}

type StepBatch struct {
	SchemaVersion       int                 `json:"schema_version"`
	Steps               []Step              `json:"steps"`
	Questions           []Question          `json:"questions"`
	RequestedChanges    []RequestedChange   `json:"requested_changes"`
	ResponsesToFindings []ResponseToFinding `json:"responses_to_findings"`
}

type Finding struct {
	ID              string   `json:"id,omitempty"`
	Severity        string   `json:"severity"`
	TargetID        string   `json:"target_id"`
	Problem         string   `json:"problem"`
	Evidence        []string `json:"evidence"`
	RequestedChange string   `json:"requested_change"`
}

type Disposition struct {
	FindingID string   `json:"finding_id"`
	Status    string   `json:"status"`
	Reason    string   `json:"reason"`
	Evidence  []string `json:"evidence"`
}

type Coverage struct {
	RequirementID    string   `json:"requirement_id"`
	CriterionIDs     []string `json:"criterion_ids"`
	TargetIDs        []string `json:"target_ids"`
	VerificationRefs []string `json:"verification_refs"`
	Status           string   `json:"status"`
}

type SourceAssessment struct {
	SourceID string `json:"source_id"`
	Role     string `json:"role"`
	Verdict  string `json:"verdict"`
	Note     string `json:"note"`
}

type Review struct {
	SchemaVersion     int                `json:"schema_version"`
	Verdict           string             `json:"verdict"`
	Summary           string             `json:"summary"`
	Findings          []Finding          `json:"findings"`
	Dispositions      []Disposition      `json:"dispositions"`
	Coverage          []Coverage         `json:"coverage"`
	SourceAssessments []SourceAssessment `json:"source_assessments"`
	Questions         []Question         `json:"questions"`
}

type Answer struct {
	QuestionID string   `json:"question_id"`
	Answer     string   `json:"answer"`
	Files      []string `json:"files,omitempty"`
}

type Answers struct {
	SchemaVersion int      `json:"schema_version"`
	Answers       []Answer `json:"answers"`
}

// Parse validates against the schema and decodes into the Go type for kind.
func Parse[T any](k Kind, data []byte) (*T, error) {
	if err := Validate(k, data); err != nil {
		return nil, err
	}
	var v T
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%s: %w", k, err)
	}
	return &v, nil
}

// ---- semantic checks ----

// Problems collects human-readable semantic violations.
type Problems []string

func (p Problems) Err() error {
	if len(p) == 0 {
		return nil
	}
	return fmt.Errorf("semantic check failed:\n  - %s", strings.Join(p, "\n  - "))
}

// CheckRequirements: unique requirement ids, unique criterion ids, criterion ids prefixed by their requirement.
func CheckRequirements(reqs []Requirement) Problems {
	var p Problems
	seenR, seenC := map[string]bool{}, map[string]bool{}
	for _, r := range reqs {
		if seenR[r.ID] {
			p = append(p, "duplicate requirement id "+r.ID)
		}
		seenR[r.ID] = true
		for _, c := range r.Criteria {
			if !strings.HasPrefix(c.ID, r.ID+".C") {
				p = append(p, fmt.Sprintf("criterion %s does not belong to %s", c.ID, r.ID))
			}
			if seenC[c.ID] {
				p = append(p, "duplicate criterion id "+c.ID)
			}
			seenC[c.ID] = true
		}
	}
	return p
}

// CriterionIDs returns all criterion ids of mandatory requirements (the set that must be assigned/covered).
func CriterionIDs(reqs []Requirement, mandatoryOnly bool) []string {
	var out []string
	for _, r := range reqs {
		if mandatoryOnly && !r.Mandatory {
			continue
		}
		for _, c := range r.Criteria {
			out = append(out, c.ID)
		}
	}
	sort.Strings(out)
	return out
}

// CheckOutline: unique step ids, depends_on resolve and are acyclic, criterion ids exist,
// every mandatory criterion is assigned to a step or to final verification, and every step
// references at least one criterion. Whether a step is actually needed is the reviewer's call.
func CheckOutline(o *Outline, reqs []Requirement) Problems {
	var p Problems
	known := map[string]bool{}
	for _, c := range CriterionIDs(reqs, false) {
		known[c] = true
	}
	steps := map[string]OutlineStep{}
	for _, s := range o.Steps {
		if _, dup := steps[s.ID]; dup {
			p = append(p, "duplicate step id "+s.ID)
		}
		steps[s.ID] = s
	}
	assigned := map[string]bool{}
	for _, s := range o.Steps {
		for _, d := range s.DependsOn {
			if d == s.ID {
				p = append(p, s.ID+" depends on itself")
			} else if _, ok := steps[d]; !ok {
				p = append(p, fmt.Sprintf("%s depends on unknown step %s", s.ID, d))
			}
		}
		if len(s.CriterionIDs) == 0 {
			p = append(p, s.ID+" references no criterion")
		}
		for _, c := range s.CriterionIDs {
			if !known[c] {
				p = append(p, fmt.Sprintf("%s references unknown criterion %s", s.ID, c))
			}
			assigned[c] = true
		}
	}
	for _, c := range o.FinalVerificationCriterionIDs {
		if !known[c] {
			p = append(p, "final verification references unknown criterion "+c)
		}
		assigned[c] = true
	}
	for _, c := range CriterionIDs(reqs, true) {
		if !assigned[c] {
			p = append(p, "mandatory criterion "+c+" is not assigned to any step or final verification")
		}
	}
	if cyc := findCycle(steps); cyc != "" {
		p = append(p, "dependency cycle: "+cyc)
	}
	return p
}

func findCycle(steps map[string]OutlineStep) string {
	const (
		white, grey, black = 0, 1, 2
	)
	color := map[string]int{}
	ids := make([]string, 0, len(steps))
	for id := range steps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var path []string
	var visit func(string) string
	visit = func(id string) string {
		color[id] = grey
		path = append(path, id)
		deps := append([]string(nil), steps[id].DependsOn...)
		sort.Strings(deps)
		for _, d := range deps {
			if _, ok := steps[d]; !ok {
				continue
			}
			switch color[d] {
			case grey:
				return strings.Join(append(path, d), " -> ")
			case white:
				if c := visit(d); c != "" {
					return c
				}
			}
		}
		path = path[:len(path)-1]
		color[id] = black
		return ""
	}
	for _, id := range ids {
		if color[id] == white {
			if c := visit(id); c != "" {
				return c
			}
		}
	}
	return ""
}

// CheckStep: the detailed step matches the approved outline entry — criteria are kept, requirement
// ids exist, verification ids are unique, and depends_on must equal the outline's set exactly: every id
// must exist in the outline, no self-reference, no duplicates, nothing dropped and nothing added. Any
// dependency change goes through requested_changes and a new outline approval, so sequential detailing
// can never assemble a cycle the outline never approved.
func CheckStep(s *Step, outline *Outline, reqs []Requirement) Problems {
	var p Problems
	var os *OutlineStep
	stepsByID := map[string]OutlineStep{}
	for i := range outline.Steps {
		stepsByID[outline.Steps[i].ID] = outline.Steps[i]
		if outline.Steps[i].ID == s.ID {
			os = &outline.Steps[i]
		}
	}
	if os == nil {
		return Problems{"step " + s.ID + " is not in the outline"}
	}
	known := map[string]bool{}
	reqIDs := map[string]bool{}
	for _, r := range reqs {
		reqIDs[r.ID] = true
		for _, c := range r.Criteria {
			known[c.ID] = true
		}
	}
	for _, c := range s.CriterionIDs {
		if !known[c] {
			p = append(p, fmt.Sprintf("%s references unknown criterion %s", s.ID, c))
		}
	}
	for _, c := range os.CriterionIDs {
		if !contains(s.CriterionIDs, c) {
			p = append(p, fmt.Sprintf("%s drops criterion %s assigned by the outline", s.ID, c))
		}
	}
	for _, r := range s.RequirementIDs {
		if !reqIDs[r] {
			p = append(p, fmt.Sprintf("%s references unknown requirement %s", s.ID, r))
		}
	}
	seenDep := map[string]bool{}
	for _, d := range s.DependsOn {
		switch {
		case d == s.ID:
			p = append(p, s.ID+" depends on itself")
		case seenDep[d]:
			p = append(p, fmt.Sprintf("%s lists dependency %s twice", s.ID, d))
		default:
			if _, ok := stepsByID[d]; !ok {
				p = append(p, fmt.Sprintf("%s depends on unknown step %s", s.ID, d))
			}
		}
		seenDep[d] = true
	}
	for _, d := range os.DependsOn {
		if !contains(s.DependsOn, d) {
			p = append(p, fmt.Sprintf("%s drops dependency %s approved in the outline (use requested_changes)", s.ID, d))
		}
	}
	for _, d := range s.DependsOn {
		if d != s.ID && !contains(os.DependsOn, d) {
			p = append(p, fmt.Sprintf("%s adds dependency %s that the approved outline does not have (use requested_changes)", s.ID, d))
		}
	}
	// acyclicity of the outline graph with this step's dependencies substituted
	merged := map[string]OutlineStep{}
	for id, st := range stepsByID {
		merged[id] = st
	}
	me := *os
	me.DependsOn = s.DependsOn
	merged[s.ID] = me
	if cyc := findCycle(merged); cyc != "" {
		p = append(p, "dependency cycle: "+cyc)
	}
	seenV := map[string]bool{}
	for _, v := range s.Verification {
		if seenV[v.ID] {
			p = append(p, fmt.Sprintf("%s: duplicate verification id %s", s.ID, v.ID))
		}
		seenV[v.ID] = true
	}
	return p
}

// CoverageScope tells the gate what "complete" means for this stage.
type CoverageScope struct {
	// ExpectedCriteria maps every requirement id that must appear to the criterion ids that must be
	// listed as covered for it. Nil criteria set = only the row is required (research/outline stages).
	ExpectedCriteria map[string][]string
	// KnownTargets / KnownVerifications, when non-nil, are the only ids a row may reference.
	KnownTargets       map[string]bool
	KnownVerifications map[string]bool
	// RequireEvidence demands non-empty target_ids and verification_refs on covered rows (detail/integration).
	RequireEvidence bool
}

// CoverageGate applies the mechanical coverage rule for a simple expected-id set: rows must match the
// set exactly, every row must be "covered", and a covered row must carry criteria, targets and
// verification references (a bare "covered" is not evidence). Use CoverageGateStrict when the expected
// criteria and the id registries are known.
func CoverageGate(rev *Review, expected []string) Problems {
	scope := CoverageScope{ExpectedCriteria: map[string][]string{}, RequireEvidence: true}
	for _, id := range expected {
		scope.ExpectedCriteria[id] = nil
	}
	return CoverageGateStrict(rev, scope)
}

// CoverageGateStrict checks the coverage table against a stage scope. Empty result = passes.
func CoverageGateStrict(rev *Review, scope CoverageScope) Problems {
	var p Problems
	if len(scope.ExpectedCriteria) > 0 && len(rev.Coverage) == 0 {
		return Problems{"coverage is empty while the registry is not"}
	}
	seen := map[string]bool{}
	for _, c := range rev.Coverage {
		id := c.RequirementID
		if seen[id] {
			p = append(p, "duplicate coverage row for "+id)
			continue
		}
		seen[id] = true
		wantCrit, known := scope.ExpectedCriteria[id]
		if !known {
			p = append(p, "coverage row for unknown requirement "+id)
			continue
		}
		if c.Status != "covered" {
			p = append(p, fmt.Sprintf("%s is %s (must be covered)", id, c.Status))
			continue
		}
		if len(c.CriterionIDs) == 0 {
			p = append(p, id+" is covered without any criterion ids")
		}
		for name, list := range map[string][]string{"criterion": c.CriterionIDs, "target": c.TargetIDs, "verification": c.VerificationRefs} {
			if dup := firstDuplicate(list); dup != "" {
				p = append(p, fmt.Sprintf("%s lists %s %s twice", id, name, dup))
			}
		}
		if scope.RequireEvidence {
			if len(c.TargetIDs) == 0 {
				p = append(p, id+" is covered without target ids")
			}
			if len(c.VerificationRefs) == 0 {
				p = append(p, id+" is covered without verification references")
			}
		}
		if wantCrit != nil {
			for _, w := range wantCrit {
				if !contains(c.CriterionIDs, w) {
					p = append(p, fmt.Sprintf("%s: criterion %s is not covered", id, w))
				}
			}
			for _, got := range c.CriterionIDs {
				if !contains(wantCrit, got) {
					p = append(p, fmt.Sprintf("%s: criterion %s does not belong to this requirement", id, got))
				}
			}
		} else {
			for _, got := range c.CriterionIDs {
				if !strings.HasPrefix(got, id+".C") {
					p = append(p, fmt.Sprintf("%s: criterion %s does not belong to this requirement", id, got))
				}
			}
		}
		if scope.KnownTargets != nil {
			for _, t := range c.TargetIDs {
				if !scope.KnownTargets[t] {
					p = append(p, fmt.Sprintf("%s references unknown target %s", id, t))
				}
			}
		}
		if scope.KnownVerifications != nil {
			for _, v := range c.VerificationRefs {
				if !scope.KnownVerifications[v] {
					p = append(p, fmt.Sprintf("%s references unknown verification %s", id, v))
				}
			}
		}
	}
	ids := make([]string, 0, len(scope.ExpectedCriteria))
	for id := range scope.ExpectedCriteria {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if !seen[id] {
			p = append(p, "requirement "+id+" has no coverage row")
		}
	}
	return p
}

// ScopeForRequirements builds the integration scope: every mandatory requirement with all its criteria.
func ScopeForRequirements(reqs []Requirement, mandatoryOnly bool) CoverageScope {
	scope := CoverageScope{ExpectedCriteria: map[string][]string{}, RequireEvidence: true}
	for _, r := range reqs {
		if mandatoryOnly && !r.Mandatory {
			continue
		}
		var crit []string
		for _, c := range r.Criteria {
			crit = append(crit, c.ID)
		}
		scope.ExpectedCriteria[r.ID] = crit
	}
	return scope
}

// OpenBlocking reports whether the review carries any blocker/major finding.
func OpenBlocking(rev *Review) bool {
	for _, f := range rev.Findings {
		if f.Severity == "blocker" || f.Severity == "major" {
			return true
		}
	}
	return false
}

func firstDuplicate(list []string) string {
	seen := map[string]bool{}
	for _, x := range list {
		if seen[x] {
			return x
		}
		seen[x] = true
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
