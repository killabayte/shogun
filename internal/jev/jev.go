// Package jev is a minimal client for TypeSafe AI's System One API ("Jev"): a state and a map of
// typed questions in, calibrated probabilities out, no generated text. Shogun uses it as an
// advisory classifier (docs/plans/shogun-jev.md). The client enforces the documented limits before
// sending, validates the response shape, never retries and never logs the key.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the official endpoint (docs.typesafe.ai/api).
	DefaultBaseURL = "https://api.typesafe.ai/v1/systemone"
	// DefaultModel is pinned: aliases such as jev-latest move on their own.
	DefaultModel = "jev-1.13.0"
	// DefaultKeyEnv is the provider's own convention for the API key variable.
	DefaultKeyEnv = "TYPESAFE_API_KEY"
	// MaxStateTokens is the documented state budget of Jev 1.13 (32k of a 64k request).
	MaxStateTokens = 32_000
	// MaxQuestions per request is Shogun's own rule for atomic, per-item requests.
	MaxQuestions = 8
	// DefaultTimeout bounds one request; Jev answers in well under a second.
	DefaultTimeout = 10 * time.Second

	charsPerToken = 4
)

// Question is one typed question. Criteria is a map option→description for a choice, an ordered
// slice of level descriptions for a score, nil for a noul.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Noul asks a yes/no question and gets P(true).
func Noul(instructions string) Question { return Question{Type: "noul", Instructions: instructions} }

// Choice asks for one of the options; every option maps to a short description.
func Choice(instructions string, options map[string]string) Question {
	return Question{Type: "choice", Instructions: instructions, Criteria: options}
}

// Score asks for a position on an ordered rubric, levels described from low to high.
func Score(instructions string, levels []string) Question {
	return Question{Type: "score", Instructions: instructions, Criteria: levels}
}

// Answer is one question's result as the API returns it.
type Answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
}

// P is the probability of an outcome: "true"/"false" for a noul, an option for a choice, a level
// index ("0", "1", …) for a score. Unknown outcomes are 0.
func (a Answer) P(outcome string) float64 {
	if a.Type == "noul" && a.Noul != nil {
		switch outcome {
		case "true":
			return *a.Noul
		case "false":
			return 1 - *a.Noul
		}
		return 0
	}
	return a.Probabilities[outcome]
}

// Usage is what the API charges for (input only; output is free).
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Result is one successful request.
type Result struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
	Latency time.Duration     `json:"latency"`
}

// Class sorts failures for the caller: none of them is retried by the client.
type Class string

const (
	ClassAuth       Class = "auth"       // no key or 401
	ClassInvalid    Class = "invalid"    // 422: the API rejected the request
	ClassRate       Class = "rate"       // 429
	ClassOverloaded Class = "overloaded" // 529
	ClassTimeout    Class = "timeout"    // the request did not finish within Timeout
	ClassShape      Class = "shape"      // the response is not what the contract promises
	ClassTransport  Class = "transport"  // connection failures and other statuses
	ClassLimit      Class = "limit"      // Shogun refused to send: a documented limit would be exceeded
)

// Error is a classified failure. Its text never contains the key.
type Error struct {
	Class  Class
	Status int
	Msg    string
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("jev %s (HTTP %d): %s", e.Class, e.Status, e.Msg)
	}
	return fmt.Sprintf("jev %s: %s", e.Class, e.Msg)
}

// Client talks to one endpoint with one key and one pinned model.
type Client struct {
	BaseURL string
	Model   string
	Key     string
	Timeout time.Duration
	HTTP    *http.Client
}

// New returns a client for the official endpoint with the pinned model.
func New(key string) *Client {
	return &Client{BaseURL: DefaultBaseURL, Model: DefaultModel, Key: key, Timeout: DefaultTimeout}
}

var reName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// Ask sends one request. Limits are checked first (ClassLimit), then the response is validated
// against the questions (ClassShape): every question answered, with its type, its values in range.
func (c *Client) Ask(ctx context.Context, state any, questions map[string]Question) (*Result, error) {
	if c.Key == "" {
		return nil, &Error{Class: ClassAuth, Msg: "no API key"}
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return nil, &Error{Class: ClassLimit, Msg: "state is not serializable: " + err.Error()}
	}
	if err := checkLimits(stateJSON, questions); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(struct {
		Model     string              `json:"model"`
		State     json.RawMessage     `json:"state"`
		Questions map[string]Question `json:"questions"`
	}{c.model(), stateJSON, questions})

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, c.baseURL(), bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Class: ClassTransport, Msg: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	start := time.Now()
	resp, err := httpc.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, &Error{Class: ClassTimeout, Msg: fmt.Sprintf("no answer within %s", timeout)}
		}
		return nil, &Error{Class: ClassTransport, Msg: scrub(err.Error(), c.Key)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	latency := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Class: classOf(resp.StatusCode), Status: resp.StatusCode, Msg: apiMessage(raw)}
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, &Error{Class: ClassShape, Msg: "response is not JSON: " + err.Error()}
	}
	res.Latency = latency
	if err := validate(&res, questions); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) model() string {
	if c.Model == "" {
		return DefaultModel
	}
	return c.Model
}

func (c *Client) baseURL() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return c.BaseURL
}

// EstimateTokens is the client's state-size estimate (4 characters per token).
func EstimateTokens(stateJSON []byte) int {
	return (len(stateJSON) + charsPerToken - 1) / charsPerToken
}

func checkLimits(stateJSON []byte, questions map[string]Question) error {
	if n := EstimateTokens(stateJSON); n > MaxStateTokens {
		return &Error{Class: ClassLimit, Msg: fmt.Sprintf("state is about %d tokens, above the %d-token budget", n, MaxStateTokens)}
	}
	if len(questions) == 0 || len(questions) > MaxQuestions {
		return &Error{Class: ClassLimit, Msg: fmt.Sprintf("%d question(s); 1 to %d per request", len(questions), MaxQuestions)}
	}
	for name, q := range questions {
		if !reName.MatchString(name) {
			return &Error{Class: ClassLimit, Msg: fmt.Sprintf("question name %q is not an identifier of at most 64 characters", name)}
		}
		if strings.TrimSpace(q.Instructions) == "" {
			return &Error{Class: ClassLimit, Msg: fmt.Sprintf("question %s has no instructions", name)}
		}
		switch q.Type {
		case "noul":
		case "choice":
			opts, ok := q.Criteria.(map[string]string)
			if !ok || len(opts) < 2 || len(opts) > 255 {
				return &Error{Class: ClassLimit, Msg: fmt.Sprintf("question %s: a choice needs 2 to 255 options", name)}
			}
		case "score":
			levels, ok := q.Criteria.([]string)
			if !ok || len(levels) < 2 || len(levels) > 10 {
				return &Error{Class: ClassLimit, Msg: fmt.Sprintf("question %s: a score needs 2 to 10 levels", name)}
			}
		default:
			return &Error{Class: ClassLimit, Msg: fmt.Sprintf("question %s: unknown type %q", name, q.Type)}
		}
	}
	return nil
}

func validate(res *Result, questions map[string]Question) error {
	if res.Model == "" {
		return &Error{Class: ClassShape, Msg: "response names no model"}
	}
	for name, q := range questions {
		a, ok := res.Answers[name]
		if !ok {
			return &Error{Class: ClassShape, Msg: "no answer to " + name}
		}
		if a.Type != q.Type {
			return &Error{Class: ClassShape, Msg: fmt.Sprintf("%s answered as %s, asked as %s", name, a.Type, q.Type)}
		}
		switch q.Type {
		case "noul":
			if a.Noul == nil || *a.Noul < 0 || *a.Noul > 1 {
				return &Error{Class: ClassShape, Msg: name + ": noul missing or outside [0,1]"}
			}
		case "choice":
			opts := q.Criteria.(map[string]string)
			if _, known := opts[a.Choice]; !known || len(a.Probabilities) == 0 {
				return &Error{Class: ClassShape, Msg: fmt.Sprintf("%s: choice %q is not an option, or no probabilities", name, a.Choice)}
			}
		case "score":
			levels := q.Criteria.([]string)
			if a.Score == nil || *a.Score < 0 || *a.Score > float64(len(levels)-1) || len(a.Probabilities) == 0 {
				return &Error{Class: ClassShape, Msg: name + ": score missing, outside its levels, or no probabilities"}
			}
		}
	}
	return nil
}

func classOf(status int) Class {
	switch status {
	case http.StatusUnauthorized:
		return ClassAuth
	case http.StatusUnprocessableEntity:
		return ClassInvalid
	case http.StatusTooManyRequests:
		return ClassRate
	case 529:
		return ClassOverloaded
	}
	return ClassTransport
}

// apiMessage extracts {"error":{"type","message"}} when present, else a short excerpt of the body.
func apiMessage(raw []byte) string {
	var e struct {
		Error struct {
			Type, Message string
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		if e.Error.Type != "" {
			return e.Error.Type + ": " + e.Error.Message
		}
		return e.Error.Message
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "empty response body"
	}
	return s
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// scrub removes the key from a message, should a transport error ever echo the request.
func scrub(msg, key string) string {
	if key == "" {
		return msg
	}
	return strings.ReplaceAll(msg, key, "<key>")
}

// Redact describes a key without revealing it: its prefix and length.
func Redact(key string) string {
	if key == "" {
		return "empty"
	}
	n := 4
	if len(key) < n {
		n = len(key)
	}
	return fmt.Sprintf("%s… (%d chars)", key[:n], len(key))
}
