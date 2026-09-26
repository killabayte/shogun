// Package provider runs the model CLIs (claude -p, codex exec) as isolated subprocesses under the
// contract recorded in docs/cli-compatibility.md and turns their streams into a validated Result
// or a classified Error. Nothing here decides plan gates; a failed call is never an approval.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Unknown marks a value the CLI did not report. It is never a measurement.
const Unknown = "unknown"

// MaxAttempts is the physical attempts per logical call (transport retries plus at most one
// format correction).
const MaxAttempts = 3

// Request is one logical model call.
type Request struct {
	Dir      string    // call directory; attempt-N/ subdirectories are created inside
	Model    string    // opaque model id or CLI alias
	Effort   string    // reasoning effort
	Prompt   string    // sent on stdin, closed with EOF
	Schema   []byte    // self-contained JSON Schema (schema.Bundle) the payload must satisfy
	Roots    []string  // readable roots; Roots[0] is the working directory
	Web      bool      // research only: allow web fetch/search
	Deadline time.Time // hard deadline for all attempts together
	// MaxAttempts caps physical attempts for this call (0 = MaxAttempts); the caller passes what is
	// left of its budget so retries never spend past an explicit limit.
	MaxAttempts int
}

// Reported is what the CLI said it actually used.
type Reported struct {
	Model             string `json:"model"`
	Effort            string `json:"effort"`
	ApprovalPolicy    string `json:"approval_policy"`
	PermissionProfile string `json:"permission_profile"`
}

// Result is a successful, schema-valid call.
type Result struct {
	Payload   json.RawMessage `json:"payload"`
	Requested Reported        `json:"requested"`
	Reported  Reported        `json:"reported"`
	Usage     json.RawMessage `json:"usage,omitempty"`
	Finish    string          `json:"finish"`
	Degraded  []string        `json:"degraded,omitempty"` // non-fatal signals worth archiving
	Attempts  int             `json:"attempts"`
	Dir       string          `json:"dir"` // directory of the accepted attempt
	// ActiveSeconds is set by the caller that measured the call (not by the adapter).
	ActiveSeconds float64 `json:"active_seconds,omitempty"`
}

// Class tells callers what to do with a failure.
type Class string

const (
	ClassTransport Class = "transport"  // crash, truncated stream, EOF: retried
	ClassPayload   Class = "payload"    // missing/invalid structured output: one format correction
	ClassRateLimit Class = "rate_limit" // subscription limit: stop and report, never retried here
	ClassConfig    Class = "config"     // auth, unknown model, bad arguments: not retried
	ClassRefusal   Class = "refusal"    // model refused: not retried
	ClassProtocol  Class = "protocol"   // contract violation (spawn, wrong model/effort/policy, write tool)
	ClassTimeout   Class = "timeout"    // hard deadline reached
	ClassCanceled  Class = "canceled"   // SIGINT / caller cancel
)

// Error is a failed call. Attempts counts physical attempts spent (for budgets).
type Error struct {
	Class    Class
	Msg      string
	Attempts int
	Dir      string // directory of the last attempt
	// BudgetStopped: the retry or format correction the policy allows was not attempted because
	// Request.MaxAttempts ran out (the caller's budget, not the failure itself, ended the call).
	BudgetStopped bool
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Class, e.Msg) }

func fail(c Class, format string, a ...any) *Error {
	return &Error{Class: c, Msg: fmt.Sprintf(format, a...)}
}

// Runner is implemented by both adapters.
type Runner interface {
	Run(ctx context.Context, req Request) (*Result, error)
}

// attemptFunc runs one physical attempt in dir. correction is the previous payload error, if any.
type attemptFunc func(ctx context.Context, dir string, correction string) (*Result, *Error)

// runAttempts is the shared retry loop: transport failures are retried, a payload failure gets one
// correction attempt, everything else stops at once. All attempts share req.Deadline.
func runAttempts(ctx context.Context, req Request, attempt attemptFunc) (*Result, error) {
	if !req.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, req.Deadline)
		defer cancel()
	}
	if err := os.MkdirAll(req.Dir, 0o700); err != nil {
		return nil, &Error{Class: ClassConfig, Msg: err.Error()}
	}
	correction, corrected := "", false
	var last *Error
	limit := MaxAttempts
	if req.MaxAttempts > 0 && req.MaxAttempts < limit {
		limit = req.MaxAttempts
	}
	for n := 1; n <= limit; n++ {
		if c := ctxClass(ctx); c != "" {
			return nil, &Error{Class: c, Msg: "no attempt started: " + ctx.Err().Error(), Attempts: n - 1, Dir: dirOf(last)}
		}
		dir := filepath.Join(req.Dir, fmt.Sprintf("attempt-%d", n))
		if err := os.Mkdir(dir, 0o700); err != nil {
			return nil, &Error{Class: ClassConfig, Msg: "attempt directory: " + err.Error(), Attempts: n - 1}
		}
		res, err := attempt(ctx, dir, correction)
		// A process that answers after TERM, or a result that lands after the deadline, is not accepted.
		if c := ctxClass(ctx); c != "" {
			return nil, &Error{Class: c, Msg: ctx.Err().Error(), Attempts: n, Dir: dir}
		}
		if err == nil {
			res.Attempts, res.Dir = n, dir
			return res, nil
		}
		err.Attempts, err.Dir = n, dir
		last = err
		retry := err.Class == ClassTransport || (err.Class == ClassPayload && !corrected)
		if !retry {
			return nil, err
		}
		if n == limit && limit < MaxAttempts {
			err.BudgetStopped = true
			return nil, err
		}
		if err.Class == ClassPayload {
			corrected, correction = true, err.Msg
		}
	}
	return nil, last
}

func ctxClass(ctx context.Context) Class {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return ClassTimeout
	case ctx.Err() != nil:
		return ClassCanceled
	}
	return ""
}

func dirOf(e *Error) string {
	if e == nil {
		return ""
	}
	return e.Dir
}

// correctionNote is appended to the prompt of a format-correction attempt.
func correctionNote(msg string) string {
	return "\n\nYour previous answer was rejected by the output contract: " + msg +
		"\nReturn only a document that satisfies the output schema.\n"
}

// ValidatePayload checks payload against a self-contained schema (the adapters apply it to every answer).
func ValidatePayload(schemaDoc, payload []byte) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaDoc))
	if err != nil {
		return fmt.Errorf("output schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("output.json", doc); err != nil {
		return err
	}
	s, err := c.Compile("output.json")
	if err != nil {
		return fmt.Errorf("output schema: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("payload is not JSON: %w", err)
	}
	return s.Validate(inst)
}
