package pipeline

import (
	"fmt"
	"sort"

	"github.com/killabayte/shogun/internal/planning/schema"
	"github.com/killabayte/shogun/internal/run"
)

// applyReview folds a review into the ledger (§6): dispositions close or keep open findings, a new
// finding (id null) gets the next F-NNN, a restated one keeps its id. It returns mechanical notes:
// dispositions for unknown/closed ids, and open findings in scope that the review did not address
// (they stay open). scope is the stage's relevant open findings, taken before the review.
func applyReview(p *run.Progress, stage, where string, rev *schema.Review, scope []run.Finding) []string {
	var notes []string
	// Indices, not pointers: appending a new finding may move the backing array.
	byID := map[string]int{}
	for i := range p.Ledger {
		byID[p.Ledger[i].ID] = i
	}
	addressed := map[string]bool{}
	for _, d := range rev.Dispositions {
		i, ok := byID[d.FindingID]
		if !ok || p.Ledger[i].Status != "open" {
			notes = append(notes, fmt.Sprintf("review has a disposition for %s, which is not an open finding", d.FindingID))
			continue
		}
		addressed[d.FindingID] = true
		if d.Status == "resolved" || d.Status == "rejected" {
			f := &p.Ledger[i]
			f.Status, f.ClosedIn, f.Reason = d.Status, where, d.Reason
		}
	}
	for _, nf := range rev.Findings {
		if i, ok := byID[nf.ID]; nf.ID != "" && ok {
			// Restating a finding keeps its id; restating a closed one reopens it.
			f := &p.Ledger[i]
			addressed[f.ID] = true
			f.Severity, f.Problem, f.RequestedChange, f.Evidence = nf.Severity, nf.Problem, nf.RequestedChange, nf.Evidence
			f.Status, f.ClosedIn, f.Reason = "open", "", ""
			continue
		}
		p.NextFinding++
		p.Ledger = append(p.Ledger, run.Finding{ID: fmt.Sprintf("F-%03d", p.NextFinding), Stage: stage,
			Severity: nf.Severity, TargetID: nf.TargetID, Problem: nf.Problem, RequestedChange: nf.RequestedChange,
			Evidence: nf.Evidence, Status: "open", OpenedIn: where})
		addressed[p.Ledger[len(p.Ledger)-1].ID] = true
	}
	for _, f := range scope {
		if !addressed[f.ID] {
			notes = append(notes, fmt.Sprintf("open finding %s has no disposition in the review; it stays open", f.ID))
		}
	}
	return notes
}

// recordDisputes counts planner disputes of findings that the next review left open.
func recordDisputes(p *run.Progress, responses []schema.ResponseToFinding) {
	for _, r := range responses {
		if r.Action != "disputed" {
			continue
		}
		for i := range p.Ledger {
			if p.Ledger[i].ID == r.FindingID && p.Ledger[i].Status == "open" {
				p.Ledger[i].Disputes++
			}
		}
	}
}

func openFindings(p *run.Progress) []run.Finding {
	var out []run.Finding
	for _, f := range p.Ledger {
		if f.Status == "open" {
			out = append(out, f)
		}
	}
	return out
}

// openSignature is the sorted set of open blocker/major findings (for the stalemate rule).
func openSignature(p *run.Progress) string {
	var ids []string
	for _, f := range p.Ledger {
		if f.Status == "open" && (f.Severity == "blocker" || f.Severity == "major") {
			ids = append(ids, f.ID)
		}
	}
	sort.Strings(ids)
	return fmt.Sprint(ids)
}
