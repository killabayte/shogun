package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/killabayte/shogun/internal/pipeline"
	"github.com/killabayte/shogun/internal/run"
)

// runStats is one run as stats reads it: its state and the model pair of its config snapshot.
type runStats struct {
	id, planner, reviewer, mode string
	st                          *run.State
	damaged                     string // why the run could not be read; empty when st is set
}

// usage classifies a run's token accounting: "ok", "incomplete" (some attempt reported no usage)
// or "unknown" (model attempts but no usage recorded, e.g. runs from before usage tracking).
func (r runStats) usage() string {
	c := r.st.Counters
	switch {
	case c.UsageIncomplete:
		return "incomplete"
	case c.Attempts > 0 && c.InputTokens+c.CacheReadTokens+c.CacheWriteTokens+c.OutputTokens == 0:
		return "unknown"
	}
	return "ok"
}

// statsRunJSON is one run in `stats --json`. Every key is always present: a damaged run has null
// where the table shows a dash, so a consumer sees the same shape for every run.
type statsRunJSON struct {
	ID               string   `json:"id"`
	Status           string   `json:"status"`
	Mode             *string  `json:"mode"`
	Planner          *string  `json:"planner"`
	Reviewer         *string  `json:"reviewer"`
	Attempts         *int     `json:"attempts"`
	ActiveSeconds    *float64 `json:"active_seconds"`
	InputTokens      *int64   `json:"input_tokens"`
	CacheReadTokens  *int64   `json:"cache_read_tokens"`
	CacheWriteTokens *int64   `json:"cache_write_tokens"`
	OutputTokens     *int64   `json:"output_tokens"`
	ReasoningTokens  *int64   `json:"reasoning_tokens"`
	CostUSD          *float64 `json:"cost_usd"`
	Usage            *string  `json:"usage"`
	Damaged          *string  `json:"damaged"`
}

// statsTotalsJSON mirrors the text totals, with the two "at least" conditions as flags.
type statsTotalsJSON struct {
	Runs             int            `json:"runs"`
	ByStatus         map[string]int `json:"by_status"`
	Damaged          int            `json:"damaged"`
	Attempts         int            `json:"attempts"`
	ActiveSeconds    float64        `json:"active_seconds"`
	InputTokens      int64          `json:"input_tokens"`
	CacheReadTokens  int64          `json:"cache_read_tokens"`
	CacheWriteTokens int64          `json:"cache_write_tokens"`
	OutputTokens     int64          `json:"output_tokens"`
	ReasoningTokens  int64          `json:"reasoning_tokens"`
	CostUSD          float64        `json:"cost_usd"`
	SpendLowerBound  bool           `json:"spend_lower_bound"`  // attempts and active time (damaged runs missing)
	TokensLowerBound bool           `json:"tokens_lower_bound"` // tokens and cost (usage missing or damaged runs)
}

// cmdStats summarises the runs under the workspace's .shogun/runs from their existing files only:
// no model calls, no locks, nothing written. Stopped and failed runs count like approved ones.
func (a *app) cmdStats(args []string) int {
	fs := a.newFlagSet("stats")
	dir := fs.String("dir", "", "runs directory to read (default: .shogun/runs in the current directory)")
	asJSON := fs.Bool("json", false, "print runs and totals as one JSON document")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	root := *dir
	if root == "" {
		root = run.RunsRoot(a.cwd)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return a.errorf("runs directory %s: %v", root, err)
	}
	var runs []runStats
	for _, e := range entries {
		if e.IsDir() {
			runs = append(runs, readRunStats(filepath.Join(root, e.Name())))
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].id < runs[j].id })
	if len(runs) == 0 && !*asJSON { // with --json an empty directory is still one document
		fmt.Fprintf(a.stderr, "no runs in %s\n", root)
		return ExitOK
	}

	tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	if !*asJSON {
		fmt.Fprintln(tw, "RUN\tSTATUS\tMODE\tPLANNER\tREVIEWER\tATTEMPTS\tACTIVE\tINPUT\tCACHE READ\tCACHE WRITE\tOUTPUT\tOF IT REASONING\tCLAUDE $\tUSAGE")
	}
	var tot run.Counters
	byStatus, damaged, partial := map[string]int{}, 0, map[string]int{}
	rows := []statsRunJSON{} // non-nil: no runs encodes as []
	for _, r := range runs {
		if r.st == nil {
			damaged++
			if *asJSON {
				rows = append(rows, statsRunJSON{ID: r.id, Status: "damaged", Damaged: &r.damaged})
			} else {
				fmt.Fprintf(tw, "%s\tdamaged\t-\t-\t-\t-\t-\t-\t-\t-\t-\t-\t-\t%s\n", r.id, r.damaged)
			}
			continue
		}
		c, mode := r.st.Counters, r.mode
		byStatus[string(r.st.Status)]++
		u := r.usage()
		if u != "ok" {
			partial[u]++
		}
		if *asJSON {
			rows = append(rows, statsRunJSON{ID: r.id, Status: string(r.st.Status), Mode: &mode, Planner: &r.planner, Reviewer: &r.reviewer,
				Attempts: &c.Attempts, ActiveSeconds: &c.ActiveSeconds, InputTokens: &c.InputTokens, CacheReadTokens: &c.CacheReadTokens,
				CacheWriteTokens: &c.CacheWriteTokens, OutputTokens: &c.OutputTokens, ReasoningTokens: &c.ReasoningTokens, CostUSD: &c.CostUSD, Usage: &u})
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%.1f min\t%d\t%d\t%d\t%d\t%d\t%.2f\t%s\n", r.id, r.st.Status, mode, dash(r.planner), dash(r.reviewer),
				c.Attempts, c.ActiveSeconds/60, c.InputTokens, c.CacheReadTokens, c.CacheWriteTokens, c.OutputTokens, c.ReasoningTokens, c.CostUSD, u)
		}
		tot.Attempts += c.Attempts
		tot.ActiveSeconds += c.ActiveSeconds
		tot.InputTokens += c.InputTokens
		tot.CacheReadTokens += c.CacheReadTokens
		tot.CacheWriteTokens += c.CacheWriteTokens
		tot.OutputTokens += c.OutputTokens
		tot.ReasoningTokens += c.ReasoningTokens
		tot.CostUSD += c.CostUSD
	}
	if *asJSON {
		totals := statsTotalsJSON{Runs: len(runs), ByStatus: byStatus, Damaged: damaged, Attempts: tot.Attempts, ActiveSeconds: tot.ActiveSeconds,
			InputTokens: tot.InputTokens, CacheReadTokens: tot.CacheReadTokens, CacheWriteTokens: tot.CacheWriteTokens, OutputTokens: tot.OutputTokens,
			ReasoningTokens: tot.ReasoningTokens, CostUSD: tot.CostUSD, SpendLowerBound: damaged > 0, TokensLowerBound: len(partial) > 0 || damaged > 0}
		b, err := json.MarshalIndent(struct {
			Runs   []statsRunJSON  `json:"runs"`
			Totals statsTotalsJSON `json:"totals"`
		}{rows, totals}, "", " ")
		if err != nil {
			return a.errorf("stats: %v", err)
		}
		fmt.Fprintln(a.stdout, string(b))
		return ExitOK
	}
	tw.Flush()

	var statuses []string
	for s, n := range byStatus {
		statuses = append(statuses, fmt.Sprintf("%d %s", n, s))
	}
	sort.Strings(statuses)
	fmt.Fprintf(a.stdout, "\ntotal: %d run(s) (%s", len(runs), strings.Join(statuses, ", "))
	if damaged > 0 {
		fmt.Fprintf(a.stdout, ", %d damaged", damaged)
	}
	spendAtLeast := "" // a damaged run's attempts and time are missing from the totals too
	if damaged > 0 {
		spendAtLeast = "at least "
	}
	fmt.Fprintf(a.stdout, "); %s%d attempt(s), %s%.1f min active\n", spendAtLeast, tot.Attempts, spendAtLeast, tot.ActiveSeconds/60)
	atLeast := ""
	if len(partial) > 0 || damaged > 0 {
		atLeast = "at least "
	}
	fmt.Fprintf(a.stdout, "tokens: %s%d input, %d cache read, %d cache write, %d output (%d of it reasoning); Claude list-price equivalent %s$%.2f (not a subscription charge)\n",
		atLeast, tot.InputTokens, tot.CacheReadTokens, tot.CacheWriteTokens, tot.OutputTokens, tot.ReasoningTokens, atLeast, tot.CostUSD)
	if len(partial) > 0 || damaged > 0 {
		fmt.Fprintf(a.stdout, "not counted: usage incomplete in %d run(s), unknown in %d run(s), %d damaged run(s)\n", partial["incomplete"], partial["unknown"], damaged)
	}
	return ExitOK
}

// runMode names the pipeline a run used. Runs created before the mode was recorded are recognised by
// their cursor, limits or drafts; a run that stopped before leaving any of them is "unknown".
func runMode(dir string, st *run.State) string {
	if st.Mode != "" {
		return st.Mode
	}
	// Every run gets empty stage directories at creation; only written drafts tell the mode.
	has := func(name string) bool {
		e, err := os.ReadDir(filepath.Join(dir, name))
		return err == nil && len(e) > 0
	}
	switch st.Cursor.Stage {
	case pipeline.StagePlan:
		return pipeline.ModeFast
	case pipeline.StageResearch, pipeline.StageOutline, pipeline.StageDetail, pipeline.StageIntegration:
		return pipeline.ModeThorough
	}
	switch {
	case has("plan") || st.Limits.Source == "fast":
		return pipeline.ModeFast
	case has("outline") || st.Limits.Source == "pre-outline" || st.Limits.Source == "derived":
		return pipeline.ModeThorough
	}
	return "unknown"
}

// readRunStats reads a run's state and config snapshot without opening or locking the run.
func readRunStats(dir string) runStats {
	r := runStats{id: filepath.Base(dir)}
	rr, err := run.Open(dir)
	if err != nil {
		r.damaged = "no state.json"
		return r
	}
	st, err := rr.LoadState()
	if err != nil {
		r.damaged = err.Error()
		return r
	}
	r.st = st
	r.mode = runMode(dir, st)
	var snap struct{ Planner, Reviewer string }
	if b, err := os.ReadFile(filepath.Join(dir, "config.snapshot.toml")); err == nil {
		var raw struct {
			Planner  string `toml:"planner"`
			Reviewer string `toml:"reviewer"`
		}
		if toml.Unmarshal(b, &raw) == nil {
			snap.Planner, snap.Reviewer = raw.Planner, raw.Reviewer
		}
	}
	r.planner, r.reviewer = snap.Planner, snap.Reviewer
	return r
}
