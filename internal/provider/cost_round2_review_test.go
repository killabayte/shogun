package provider

import (
	"context"
	"testing"
)

type costRound2RetryRunner struct{ attempts int }

func (r *costRound2RetryRunner) Run(ctx context.Context, req Request) (*Result, error) {
	return runAttempts(ctx, req, func(context.Context, string, string) (*Result, *Error) {
		r.attempts++
		return nil, fail(ClassTransport, "synthetic transport error")
	})
}

func TestCostRound2PreflightCapsPhysicalAttemptsAtTwo(t *testing.T) {
	planner, reviewer := &costRound2RetryRunner{}, &costRound2RetryRunner{}
	RunPreflight(context.Background(), planner, reviewer, Request{Model: "opus", Effort: "high"}, Request{Model: "gpt-6-astra", Effort: "high"}, t.TempDir())
	if got := planner.attempts + reviewer.attempts; got > 2 {
		t.Fatalf("doctor preflight used %d physical attempts, promised cap is 2", got)
	}
}
