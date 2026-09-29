package eval

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/killabayte/shogun/internal/jev"
)

type pr15Transport func(*http.Request) (*http.Response, error)

func (f pr15Transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Answers only from memory. All expected adverse outcomes fire, all controls are
// benign. The adapter gets complete, normalized, correctly typed responses.
func pr15PerfectClient(t *testing.T) *jev.Client {
	t.Helper()
	responses := map[string][]byte{}
	for _, cs := range Cases {
		_, items, err := Load(cs)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			answers := map[string]map[string]any{}
			for name, q := range it.Questions {
				if q.Type == "noul" {
					p := 0.05
					for _, a := range it.Adverse {
						if a.Question == name && a.Outcome == "false" {
							p = 0.95
						}
					}
					answers[name] = map[string]any{"type": "noul", "noul": p}
				} else {
					// v4 renamed the requirement choice: pick the first option no adverse spec names.
					adverseOpts := map[string]bool{}
					for _, a := range it.Adverse {
						if a.Question == name {
							for _, o := range strings.Split(a.Outcome, "|") {
								adverseOpts[o] = true
							}
						}
					}
					prob := map[string]float64{}
					pick := ""
					for _, option := range sortedOptions(q.Criteria.(map[string]string)) {
						prob[option] = 0
						if pick == "" && !adverseOpts[option] && option != "unknown" {
							pick = option
						}
					}
					prob[pick] = 1
					answers[name] = map[string]any{"type": "choice", "choice": pick, "probabilities": prob, "confidence": 1.0}
				}
			}
			for _, expected := range cs.Expect {
				for _, s := range expected.Any {
					// v4: a step's criterion checks may live in its "#crit" request; apply an expected
					// answer only to the request that carries the question.
					if s.Item != it.ID && !strings.HasPrefix(it.ID, s.Item+"#") {
						continue
					}
					a, ok := answers[s.Question]
					if !ok {
						continue
					}
					if it.Questions[s.Question].Type == "noul" {
						p := 0.95
						if s.Outcome == "false" {
							p = 0.05
						}
						a["noul"] = p
					} else {
						prob := a["probabilities"].(map[string]float64)
						for option := range prob {
							prob[option] = 0
						}
						first := strings.Split(s.Outcome, "|")[0] // a summed outcome: its first option suffices
						prob[first], a["choice"] = 1, first
					}
				}
			}
			state, _ := json.Marshal(it.State)
			body, _ := json.Marshal(map[string]any{"model": jev.DefaultModel, "answers": answers, "usage": map[string]int{"input_tokens": 100, "output_tokens": 10}})
			responses[pr15Key(state, it.Questions)] = body
		}
	}
	c := jev.New("fake-review-key")
	c.HTTP = &http.Client{Transport: pr15Transport(func(r *http.Request) (*http.Response, error) {
		var request struct {
			State     json.RawMessage         `json:"state"`
			Questions map[string]jev.Question `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		body, ok := responses[pr15Key(request.State, request.Questions)]
		if !ok {
			t.Fatal("unexpected fixture state")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Request: r, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	return c
}

// pr15Key identifies a request by its state and its question names: in v4 a step and its "#crit"
// request share the state and differ only in the questions.
func pr15Key(state []byte, questions map[string]jev.Question) string {
	names := make([]string, 0, len(questions))
	for n := range questions {
		names = append(names, n)
	}
	sort.Strings(names)
	return string(state) + "|" + strings.Join(names, ",")
}

func sortedOptions(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestPR15ReviewPerfectControl(t *testing.T) {
	r := Run(context.Background(), pr15PerfectClient(t), Cases, DefaultCaps)
	if ok, why := r.Decision(); !ok || r.Requests != 55 {
		t.Fatalf("invalid test control: %s requests=%d", why, r.Requests)
	}
}

func TestPR15ReviewDeadlineReachesInFlightRequest(t *testing.T) {
	c := pr15PerfectClient(t)
	c.Timeout = 5 * time.Minute
	transport := c.HTTP.Transport
	var deadline time.Time
	c.HTTP.Transport = pr15Transport(func(r *http.Request) (*http.Response, error) {
		deadline, _ = r.Context().Deadline()
		return transport.RoundTrip(r)
	})
	start := time.Now()
	Run(context.Background(), c, Cases[len(Cases)-1:], Caps{MaxRequests: 1, MaxElapsed: time.Minute})
	if deadline.IsZero() || deadline.After(start.Add(time.Minute+50*time.Millisecond)) {
		t.Fatalf("one-minute evaluation cap was not passed to the active request: request deadline allows %s", deadline.Sub(start))
	}
}

func TestPR15ReviewLoadErrorsCannotPass(t *testing.T) {
	for _, mode := range []string{"missing_good_controls", "missing_two_defect_cases"} {
		t.Run(mode, func(t *testing.T) {
			cases := append([]Case(nil), Cases...)
			removed := 0
			for i := range cases {
				if (mode == "missing_good_controls" && cases[i].Label == "good") || (mode == "missing_two_defect_cases" && cases[i].Label == "defective" && removed < 2) {
					cases[i].Dir = "missing-fixture"
					removed++
				}
			}
			r := Run(context.Background(), pr15PerfectClient(t), cases, DefaultCaps)
			if ok, why := r.Decision(); ok {
				t.Fatalf("missing mandatory cases accepted: %s; controls=%v", why, r.GoodFalseAlarms)
			}
		})
	}
}

func TestPR15ReviewReportRoundTripPreservesDecision(t *testing.T) {
	r := Run(context.Background(), pr15PerfectClient(t), Cases, DefaultCaps)
	before, why := r.Decision()
	if !before {
		t.Fatal("control failed: " + why)
	}
	var restored Report
	if err := json.Unmarshal(r.JSON(), &restored); err != nil {
		t.Fatal(err)
	}
	if after, why := restored.Decision(); after != before {
		t.Fatalf("saved report changes its decision after reading: %s", why)
	}
}

func TestPR15ReviewBindingAnswerDoesNotReviveAssumption(t *testing.T) {
	for _, source := range []string{"user", "answers-file"} {
		t.Run(source, func(t *testing.T) {
			var plan Plan
			if err := json.Unmarshal([]byte(`{"questions":[{"id":"Q-001","question":"Keep the fields?","proposed_assumption":"Omit the unavailable fields"}]}`), &plan); err != nil {
				t.Fatal(err)
			}
			decisions := []Decision{{Question: "Keep the fields?", Answer: "Keep every field; use null for unavailable values", Source: source}}
			for _, item := range Items("Emit the requested JSON fields", &plan, decisions) {
				if item.Kind == "assumption" {
					t.Fatalf("a binding answer revived the rejected proposal as a current assumption: %+v", item.State)
				}
			}
		})
	}
}
