package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/killabayte/shogun/internal/jev"
)

// The §7 schedule: 47 requests over 9 cases, every state within the limits, every item's questions
// within one request.
func TestEvaluationSetMatchesTheDesign(t *testing.T) {
	want := map[string]int{"5d31": 8, "1b4e": 8, "6be3": 8, "a592r1": 9, "08fdr1": 7, "102433": 4, "mutA": 1, "mutB": 1, "mutC": 1}
	total := 0
	for _, c := range Cases {
		_, items, err := Load(c)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if len(items) != want[c.Name] {
			t.Errorf("%s: %d items, want %d: %v", c.Name, len(items), want[c.Name], SortedIDs(items))
		}
		total += len(items)
		for _, it := range items {
			b, _ := json.Marshal(it.State)
			if n := jev.EstimateTokens(b); n > jev.MaxStateTokens {
				t.Errorf("%s/%s: state %d tokens", c.Name, it.ID, n)
			}
			if len(it.Questions) == 0 || len(it.Questions) > jev.MaxQuestions {
				t.Errorf("%s/%s: %d questions", c.Name, it.ID, len(it.Questions))
			}
			for _, adv := range it.Adverse {
				if _, ok := it.Questions[adv.Question]; !ok {
					t.Errorf("%s/%s: adverse spec on unknown question %s", c.Name, it.ID, adv.Question)
				}
			}
		}
		if c.Label == "defective" && len(c.Expect) == 0 || c.Label == "good" && len(c.Expect) != 0 {
			t.Errorf("%s: label %s with %d expectations", c.Name, c.Label, len(c.Expect))
		}
		for _, e := range c.Expect {
			for _, s := range e.Any {
				found := false
				for _, it := range items {
					if it.ID == s.Item {
						found = true
						if _, ok := it.Questions[s.Question]; !ok {
							t.Errorf("%s: expectation on unknown question %s/%s", c.Name, s.Item, s.Question)
						}
					}
				}
				if !found {
					t.Errorf("%s: expectation on unknown item %s", c.Name, s.Item)
				}
			}
		}
	}
	if total != 47 {
		t.Fatalf("%d requests in total, want 47", total)
	}
	defects := 0
	for _, c := range Cases {
		if c.Label == "defective" {
			defects++
		}
	}
	if defects != 7 {
		t.Fatalf("%d defective cases, want 7", defects)
	}
}

// The items carry the evidence the design promises: a step sees the text of its criteria and the
// binding decisions, a requirement sees all requirement statements, an assumption is one item even
// when the planner's question and its decision both exist.
func TestItemsCarryTheirEvidence(t *testing.T) {
	_, items, err := Load(Cases[0]) // 5d31
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Item{}
	for _, it := range items {
		byID[it.ID] = it
	}
	s2 := byID["S-002"].State.(map[string]any)
	if crit := s2["criteria"].([]string); len(crit) != 8 || !strings.HasPrefix(crit[0], "R-001.C1: ") {
		t.Fatalf("S-002 criteria %v", crit)
	}
	r1 := byID["R-001"].State.(map[string]any)
	if all := r1["all_requirements"].([]string); len(all) != 5 || !strings.HasPrefix(all[4], "R-005: ") {
		t.Fatalf("all_requirements %v", all)
	}
	if a, ok := byID["A-001"]; !ok || a.Kind != "assumption" || byID["A-002"].ID != "" {
		t.Fatalf("assumptions: %v", SortedIDs(items))
	}
	if a := byID["A-001"].State.(map[string]any)["assumption"].(map[string]string); !strings.Contains(a["answer"], "Leave both unchanged") {
		t.Fatalf("assumption %v", a)
	}
}

// fake answers every question with canned probabilities: adverse everywhere for the named items,
// benign elsewhere.
func fake(t *testing.T, adverseItems map[string]bool, delay time.Duration) (*jev.Client, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{} // a handler cut by the time cap may still run after Run returns
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(delay)
		var req struct {
			State     map[string]any          `json:"state"`
			Questions map[string]jev.Question `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		id := ""
		for _, k := range []string{"assumption", "requirement", "step"} {
			if m, ok := req.State[k].(map[string]any); ok {
				if s, ok := m["id"].(string); ok {
					id = s
				} else if k == "assumption" {
					id = "assumption"
				}
			}
		}
		adverse := adverseItems[id] || (id == "assumption" && adverseItems["A-*"])
		answers := map[string]any{}
		for name, q := range req.Questions {
			switch q.Type {
			case "noul":
				p := 0.05
				if name == "criteria_match" || name == "verification_bites" {
					p = 0.95
				}
				if adverse {
					p = 1 - p
				}
				answers[name] = map[string]any{"type": "noul", "noul": p}
			case "choice":
				opts := q.Criteria.(map[string]any)
				pick := "task_explicit"
				if _, ok := opts["role"]; ok || name == "role" {
					pick = "required_by_task"
				}
				if adverse {
					pick = "not_asked"
					if name == "role" {
						pick = "optional_improvement"
					}
				}
				probs := map[string]float64{}
				for o := range opts {
					probs[o] = 0.02
				}
				probs[pick] = 0.92
				answers[name] = map[string]any{"type": "choice", "choice": pick, "confidence": 0.9, "probabilities": probs}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 1000, "output_tokens": 10}})
	}))
	t.Cleanup(srv.Close)
	return &jev.Client{BaseURL: srv.URL, Key: "apik-test", Timeout: 5 * time.Second}, calls
}

// With a fake that flags exactly the defective items, every expectation is met, the good cases have
// no false alarms, the totals and cost add up, and the decision rule passes.
func TestRunJudgesExpectations(t *testing.T) {
	c, calls := fake(t, map[string]bool{"S-001": true, "S-002": true, "R-002": true, "S-003": true, "R-006": true, "A-*": true}, 0)
	rep := Run(context.Background(), c, Cases, DefaultCaps)
	if calls.Load() != 47 || rep.Requests != 47 || rep.Skipped != 0 || rep.Errors != 0 || rep.InputTokens != 47000 || rep.Model != "jev-1.13.0" {
		t.Fatalf("report %+v", rep)
	}
	if rep.CostUSD < 0.0019 || rep.CostUSD > 0.0020 {
		t.Fatalf("cost %f", rep.CostUSD)
	}
	if rep.DefectsCaught != 7 || rep.Defects != 7 {
		var b strings.Builder
		rep.WriteTable(&b)
		t.Fatalf("caught %d/%d\n%s", rep.DefectsCaught, rep.Defects, b.String())
	}
	// This fake flags every assumption and S-001/S-002/R-002 everywhere, so good cases do get flags
	// (that is the fake, not the classifier): the rule must then fail on false alarms.
	if ok, why := rep.Decision(); ok || !strings.Contains(why, "7 of 7 defects caught") {
		t.Fatalf("decision %v %s", ok, why)
	}
	var b strings.Builder
	rep.WriteTable(&b)
	if !strings.Contains(b.String(), "1b4e (defective): 2/2 expected signal(s) fired") || !strings.Contains(b.String(), "below the §7 rule") {
		t.Fatalf("table:\n%s", b.String())
	}
}

// A benign fake: nothing fires, every defective case is a miss with the probabilities seen, the
// good cases are clean, and the rule fails on missed defects.
func TestRunReportsMisses(t *testing.T) {
	c, _ := fake(t, nil, 0)
	rep := Run(context.Background(), c, Cases, DefaultCaps)
	if rep.DefectsCaught != 0 || rep.GoodFalseAlarms["5d31"] != 0 || rep.GoodFalseAlarms["102433"] != 0 {
		t.Fatalf("%+v", rep)
	}
	var b strings.Builder
	rep.WriteTable(&b)
	if !strings.Contains(b.String(), "08fdr1 (defective): 0/1 expected signal(s) fired; missed: S-002's tests could not detect wrong totals (the reviewer's major) [S-002/verification_bites=false 0.05]") {
		t.Fatalf("miss line:\n%s", b.String())
	}
}

// The caps stop the run: the rest is skipped, the report says why, and the decision fails.
func TestRunStopsAtCaps(t *testing.T) {
	c, calls := fake(t, nil, 0)
	rep := Run(context.Background(), c, Cases, Caps{MaxRequests: 10, MaxElapsed: time.Minute})
	if calls.Load() != 10 || rep.Requests != 10 || rep.Skipped != 37 || rep.StoppedBy != "request cap 10" {
		t.Fatalf("%+v", rep)
	}
	if ok, why := rep.Decision(); ok || !strings.Contains(why, "37 skipped") {
		t.Fatalf("%v %s", ok, why)
	}
	c, calls = fake(t, nil, 30*time.Millisecond)
	rep = Run(context.Background(), c, Cases, Caps{MaxRequests: 100, MaxElapsed: 100 * time.Millisecond})
	if calls.Load() >= 47 || rep.Skipped == 0 || !strings.HasPrefix(rep.StoppedBy, "time cap") {
		t.Fatalf("time cap: calls %d skipped %d stopped %q", calls.Load(), rep.Skipped, rep.StoppedBy)
	}
}

// A failing request is recorded and the run continues.
func TestRunRecordsErrorsAndContinues(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		w.WriteHeader(529)
	}))
	defer srv.Close()
	c := &jev.Client{BaseURL: srv.URL, Key: "k", Timeout: time.Second}
	rep := Run(context.Background(), c, Cases[:1], DefaultCaps)
	if rep.Requests != 8 || rep.Errors != 8 || !strings.Contains(rep.Cases[0].Items[0].Err, "rate") {
		t.Fatalf("%+v", rep.Cases[0].Items[0])
	}
	if b := rep.JSON(); !strings.Contains(string(b), `"error": "jev rate (HTTP 429): rate_limit_error: slow down"`) {
		t.Fatalf("json:\n%s", b)
	}
}
