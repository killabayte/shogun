package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/killabayte/shogun/internal/jev"
)

// Caps bound one evaluation run; set before the first run (§7: 60 requests, 120 s).
type Caps struct {
	MaxRequests int
	MaxElapsed  time.Duration
}

// DefaultCaps are the §7 caps.
var DefaultCaps = Caps{MaxRequests: 60, MaxElapsed: 120 * time.Second}

// Flag is one adverse outcome that fired, with the probability compared.
type Flag struct {
	Question string  `json:"question"`
	Outcome  string  `json:"outcome"`
	P        float64 `json:"p"`
	Trim     bool    `json:"trim"` // also at or beyond the trim threshold (information only)
}

// ItemResult is one request's outcome.
type ItemResult struct {
	ID        string                `json:"id"`
	Kind      string                `json:"kind"`
	Answers   map[string]jev.Answer `json:"answers,omitempty"`
	Flags     []Flag                `json:"flags"`
	Latency   time.Duration         `json:"latency"`
	Tokens    int64                 `json:"input_tokens"`
	Err       string                `json:"error,omitempty"`
	Skipped   bool                  `json:"skipped,omitempty"` // a cap stopped the run before this item
	adverseAt map[string]float64
}

// CaseResult is one case with its expectation verdicts.
type CaseResult struct {
	Name        string       `json:"name"`
	Label       string       `json:"label"`
	Items       []ItemResult `json:"items"`
	Caught      int          `json:"caught"`       // expectations met
	Expected    int          `json:"expected"`     // expectations
	Misses      []string     `json:"misses"`       // expectation notes not met, with the probabilities seen
	FalseAlarms []string     `json:"false_alarms"` // flags not covered by an expectation
	Err         string       `json:"error,omitempty"`
}

// Report is one evaluation run. Every input to Decision is in the JSON, so a saved report
// decides the same way when read back.
type Report struct {
	QuestionsVersion int           `json:"questions_version"`
	Model            string        `json:"model"`
	Cases            []CaseResult  `json:"cases"`
	Requests         int           `json:"requests"`
	Skipped          int           `json:"skipped"`
	InputTokens      int64         `json:"input_tokens"`
	CostUSD          float64       `json:"cost_usd"` // list price, $0.042 per million input tokens
	Elapsed          time.Duration `json:"elapsed"`
	Errors           int           `json:"errors"`      // failed requests
	CaseErrors       int           `json:"case_errors"` // cases that could not be loaded
	// Decision inputs (§7): defective cases judged and caught; good cases judged and their false
	// alarms at the signal threshold.
	Defects         int            `json:"defects"`
	DefectsCaught   int            `json:"defects_caught"`
	Good            int            `json:"good"`
	GoodFalseAlarms map[string]int `json:"good_false_alarms"`
	StoppedBy       string         `json:"stopped_by,omitempty"`
}

// The §7 set and rule. A run that judged fewer cases than the set, or lost any request, cannot pass.
const (
	RequiredDefects = 7
	RequiredGood    = 2
	MinCaught       = 5
	MaxFalseAlarms  = 1
)

const listPricePerMillion = 0.042

// Run asks Jev about every item of every case, sequentially, within caps. A failed request is
// recorded and the run goes on; a cap stops the run and marks the rest skipped.
func Run(ctx context.Context, c *jev.Client, cases []Case, caps Caps) *Report {
	rep := &Report{QuestionsVersion: QuestionsVersion, GoodFalseAlarms: map[string]int{}}
	start := time.Now()
	// The time cap is a deadline on every request, not only a check between requests: a request in
	// flight when the cap arrives is cut and counted as skipped.
	runCtx, cancel := context.WithDeadline(ctx, start.Add(caps.MaxElapsed))
	defer cancel()
	stopped := false
	for _, cs := range cases {
		cr := CaseResult{Name: cs.Name, Label: cs.Label}
		_, items, err := Load(cs)
		if err != nil {
			cr.Err = err.Error()
			rep.CaseErrors++
			rep.Cases = append(rep.Cases, cr)
			continue
		}
		for _, it := range items {
			ir := ItemResult{ID: it.ID, Kind: it.Kind, Flags: []Flag{}, adverseAt: map[string]float64{}}
			if stopped || rep.Requests >= caps.MaxRequests || time.Since(start) >= caps.MaxElapsed {
				if !stopped {
					stopped = true
					if rep.Requests >= caps.MaxRequests {
						rep.StoppedBy = fmt.Sprintf("request cap %d", caps.MaxRequests)
					} else {
						rep.StoppedBy = fmt.Sprintf("time cap %s", caps.MaxElapsed)
					}
				}
				ir.Skipped = true
				rep.Skipped++
				cr.Items = append(cr.Items, ir)
				continue
			}
			rep.Requests++
			res, err := c.Ask(runCtx, it.State, it.Questions)
			// The deadline is judged on its own, whatever Ask returned: an answer that arrives after
			// the cap is not evidence of a run within the cap.
			if runCtx.Err() != nil {
				stopped, rep.StoppedBy = true, fmt.Sprintf("time cap %s", caps.MaxElapsed)
				ir.Skipped = true
				rep.Skipped++
				cr.Items = append(cr.Items, ir)
				continue
			}
			if err != nil {
				ir.Err = err.Error()
				rep.Errors++
				cr.Items = append(cr.Items, ir)
				continue
			}
			rep.Model = res.Model
			rep.InputTokens += res.Usage.InputTokens
			ir.Answers, ir.Latency, ir.Tokens = res.Answers, res.Latency, res.Usage.InputTokens
			for _, adv := range it.Adverse {
				hit, p := Hit(res.Answers[adv.Question], adv)
				ir.adverseAt[adv.Question+"="+adv.Outcome] = p
				if hit {
					ir.Flags = append(ir.Flags, Flag{Question: adv.Question, Outcome: adv.Outcome, P: p, Trim: p >= TrimThreshold})
				}
			}
			cr.Items = append(cr.Items, ir)
		}
		judge(&cr, cs)
		if cs.Label == "defective" {
			rep.Defects++
			if cr.Expected > 0 && cr.Caught == cr.Expected {
				rep.DefectsCaught++
			}
		} else {
			rep.Good++
			rep.GoodFalseAlarms[cs.Name] = len(cr.FalseAlarms)
		}
		rep.Cases = append(rep.Cases, cr)
	}
	rep.Elapsed = time.Since(start)
	rep.CostUSD = float64(rep.InputTokens) / 1e6 * listPricePerMillion
	return rep
}

// judge compares a case's flags with its expectations: every expectation must be met by one of its
// alternatives; flags outside every expectation are false alarms (on a good case, every flag is).
func judge(cr *CaseResult, cs Case) {
	cr.Expected = len(cs.Expect)
	covered := map[string]bool{}
	fired := map[string]bool{}
	for _, ir := range cr.Items {
		id, _, _ := strings.Cut(ir.ID, "#") // criterion checks belong to their step
		for _, f := range ir.Flags {
			fired[id+"/"+f.Question+"="+f.Outcome] = true
		}
	}
	for _, e := range cs.Expect {
		met := false
		var seen []string
		for _, s := range e.Any {
			key := s.Item + "/" + s.Question + "=" + s.Outcome
			covered[key] = true
			if fired[key] {
				met = true
			}
			for _, ir := range cr.Items {
				if ir.ID == s.Item || strings.HasPrefix(ir.ID, s.Item+"#") {
					if p, ok := ir.adverseAt[s.Question+"="+s.Outcome]; ok {
						seen = append(seen, fmt.Sprintf("%s %.2f", key, p))
					} else if ir.Skipped {
						seen = append(seen, key+" skipped")
					} else if ir.Err != "" {
						seen = append(seen, key+" error")
					}
				}
			}
		}
		if met {
			cr.Caught++
		} else {
			cr.Misses = append(cr.Misses, e.Note+" ["+strings.Join(seen, "; ")+"]")
		}
	}
	for _, ir := range cr.Items {
		id, _, _ := strings.Cut(ir.ID, "#")
		for _, f := range ir.Flags {
			if key := id + "/" + f.Question + "=" + f.Outcome; !covered[key] {
				cr.FalseAlarms = append(cr.FalseAlarms, fmt.Sprintf("%s %.2f", key, f.P))
			}
		}
	}
	if cr.Misses == nil {
		cr.Misses = []string{}
	}
	if cr.FalseAlarms == nil {
		cr.FalseAlarms = []string{}
	}
}

// Decision applies the §7 rule to a complete run only: all 7 defective and both good cases judged,
// no case or request lost; then at least 5 defects caught and at most 1 false alarm per good case
// at the signal threshold.
func (r *Report) Decision() (ok bool, why string) {
	worst := 0
	for _, n := range r.GoodFalseAlarms {
		if n > worst {
			worst = n
		}
	}
	complete := r.CaseErrors == 0 && r.Errors == 0 && r.Skipped == 0 && r.Defects == RequiredDefects && r.Good == RequiredGood
	ok = complete && r.DefectsCaught >= MinCaught && worst <= MaxFalseAlarms
	why = fmt.Sprintf("%d of %d defects caught (%d of %d required); %d good case(s) (%d required), worst %d false alarm(s) (%d allowed); %d case error(s); %d request error(s); %d skipped",
		r.DefectsCaught, r.Defects, MinCaught, RequiredDefects, r.Good, RequiredGood, worst, MaxFalseAlarms, r.CaseErrors, r.Errors, r.Skipped)
	if !complete {
		why = "incomplete run: " + why
	}
	return ok, why
}

// WriteTable prints the report for a human: one line per item, then the case verdicts and totals.
func (r *Report) WriteTable(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CASE\tLABEL\tITEM\tKIND\tFLAGS (question=outcome p; * at trim threshold)\tMS\tTOKENS")
	for _, cs := range r.Cases {
		for _, it := range cs.Items {
			var fl []string
			for _, f := range it.Flags {
				mark := ""
				if f.Trim {
					mark = "*"
				}
				fl = append(fl, fmt.Sprintf("%s=%s %.2f%s", f.Question, f.Outcome, f.P, mark))
			}
			cell := strings.Join(fl, "; ")
			switch {
			case it.Skipped:
				cell = "skipped"
			case it.Err != "":
				cell = "ERROR " + it.Err
			case cell == "":
				cell = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\n", cs.Name, cs.Label, it.ID, it.Kind, cell, it.Latency.Milliseconds(), it.Tokens)
		}
	}
	tw.Flush()
	fmt.Fprintln(w)
	for _, cs := range r.Cases {
		if cs.Err != "" {
			fmt.Fprintf(w, "%s: ERROR %s\n", cs.Name, cs.Err)
			continue
		}
		if cs.Label == "defective" {
			fmt.Fprintf(w, "%s (defective): %d/%d expected signal(s) fired", cs.Name, cs.Caught, cs.Expected)
		} else {
			fmt.Fprintf(w, "%s (good): %d false alarm(s)", cs.Name, len(cs.FalseAlarms))
		}
		if len(cs.Misses) > 0 {
			fmt.Fprintf(w, "; missed: %s", strings.Join(cs.Misses, " | "))
		}
		if len(cs.FalseAlarms) > 0 && cs.Label == "defective" {
			fmt.Fprintf(w, "; other flags: %s", strings.Join(cs.FalseAlarms, ", "))
		} else if len(cs.FalseAlarms) > 0 {
			fmt.Fprintf(w, ": %s", strings.Join(cs.FalseAlarms, ", "))
		}
		fmt.Fprintln(w)
	}
	ok, why := r.Decision()
	verdict := "below the §7 rule"
	if ok {
		verdict = "meets the §7 rule"
	}
	fmt.Fprintf(w, "\nquestions v%d; model %s; %d request(s), %d skipped, %d error(s); %d input tokens ($%.4f list price); %.1fs", r.QuestionsVersion, r.Model, r.Requests, r.Skipped, r.Errors, r.InputTokens, r.CostUSD, r.Elapsed.Seconds())
	if r.StoppedBy != "" {
		fmt.Fprintf(w, "; stopped by %s", r.StoppedBy)
	}
	fmt.Fprintf(w, "\ndecision: %s — %s\n", verdict, why)
}

// JSON is the report as data (answers included).
func (r *Report) JSON() []byte {
	b, _ := json.MarshalIndent(r, "", " ")
	return b
}
