package eval

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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
					pick := "task_explicit"
					if name == "role" {
						pick = "required_by_task"
					}
					prob := map[string]float64{}
					for option := range q.Criteria.(map[string]string) {
						prob[option] = 0
					}
					prob[pick] = 1
					answers[name] = map[string]any{"type": "choice", "choice": pick, "probabilities": prob, "confidence": 1.0}
				}
			}
			for _, expected := range cs.Expect {
				for _, s := range expected.Any {
					if s.Item != it.ID {
						continue
					}
					a := answers[s.Question]
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
						prob[s.Outcome], a["choice"] = 1, s.Outcome
					}
				}
			}
			state, _ := json.Marshal(it.State)
			body, _ := json.Marshal(map[string]any{"model": jev.DefaultModel, "answers": answers, "usage": map[string]int{"input_tokens": 100, "output_tokens": 10}})
			responses[string(state)] = body
		}
	}
	c := jev.New("fake-review-key")
	c.HTTP = &http.Client{Transport: pr15Transport(func(r *http.Request) (*http.Response, error) {
		var request struct {
			State json.RawMessage `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		body, ok := responses[string(request.State)]
		if !ok {
			t.Fatal("unexpected fixture state")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Request: r, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	return c
}

func TestPR15ReviewPerfectControl(t *testing.T) {
	r := Run(context.Background(), pr15PerfectClient(t), Cases, DefaultCaps)
	if ok, why := r.Decision(); !ok || r.Requests != 47 {
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
