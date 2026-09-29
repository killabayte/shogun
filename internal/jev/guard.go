package jev

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The outbound guard: nothing that looks like a secret, a credential, a person's address or an
// internal identifier leaves for Jev. It runs inside Ask over the request's audit text — the
// state's strings as written and every question with its criteria (Audit) — so no caller can
// bypass it and no JSON escaping hides a quote or a line break from a pattern. It blocks the whole
// request rather than redacting: a redacted plan item is still context around a secret. Matches
// are reported by pattern name and count only; the text itself is never repeated.

// Pattern is one built-in or configured check.
type Pattern struct {
	Name string
	Re   *regexp.Regexp
}

// Match is one pattern that fired, with how many times.
type Match struct {
	Name  string
	Count int
}

// ClassSensitive is the failure class of a request the guard refused.
const ClassSensitive Class = "sensitive"

// BuiltinPatterns are always on. They are deliberately literal (no entropy heuristics): a false
// alarm costs one skipped advisory request, a miss costs a secret.
var BuiltinPatterns = []Pattern{
	{"aws_access_key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"prefixed_token", regexp.MustCompile(`\b(?:sk|apik|ghp|gho|ghu|ghs|glpat|xox[baprs])[-_][A-Za-z0-9_-]{10,}`)},
	{"google_api_key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{"private_key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"bearer_token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{12,}`)},
	{"credential_assignment", regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|secret|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret|authorization)\b["']?\s*[=:]\s*["']?[^\s"']{6,}`)},
	{"token_assignment", regexp.MustCompile(`(?i)\btoken\b["']?\s*[=:]\s*["']?[^\s"']{8,}`)},
	{"credential_url", regexp.MustCompile(`\b[a-z][a-z0-9+.-]*://[^/\s:@]+:[^@\s/]+@`)},
	{"email", regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)},
	{"ipv4", regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)},
	{"aws_account_id", regexp.MustCompile(`\b\d{12}\b`)},
	{"home_path", regexp.MustCompile(`/(?:Users|home)/[A-Za-z0-9._-]+`)},
}

// CompileDeny turns configured extra patterns (jev_deny) into checks named deny:<n>.
func CompileDeny(exprs []string) ([]Pattern, error) {
	var out []Pattern
	for i, e := range exprs {
		re, err := regexp.Compile(e)
		if err != nil {
			return nil, fmt.Errorf("jev_deny[%d] %q: %v", i, e, err)
		}
		out = append(out, Pattern{Name: fmt.Sprintf("deny:%d", i+1), Re: re})
	}
	return out, nil
}

// Scan reports every built-in and extra pattern that matches text, by name and count.
func Scan(text string, extra []Pattern) []Match {
	var out []Match
	for _, p := range append(append([]Pattern{}, BuiltinPatterns...), extra...) {
		if n := len(p.Re.FindAllStringIndex(text, -1)); n > 0 {
			out = append(out, Match{Name: p.Name, Count: n})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Describe lists matches for a log line or a report: "email×2, ipv4×1".
func Describe(ms []Match) string {
	var parts []string
	for _, m := range ms {
		parts = append(parts, fmt.Sprintf("%s×%d", m.Name, m.Count))
	}
	return strings.Join(parts, ", ")
}

// Audit renders everything a request would carry — the state's keys and strings as plain text
// (no JSON escaping, so quotes and line breaks read as written) and every question's name, type,
// instructions and criteria — one representation that the guard scans and that a human can read
// before anything is sent. The authorization header is not part of it.
func Audit(state any, questions map[string]Question) string {
	var b strings.Builder
	b.WriteString("# state\n")
	var v any
	if raw, err := json.Marshal(state); err == nil {
		json.Unmarshal(raw, &v)
	}
	flatten(&b, "", v)
	b.WriteString("# questions\n")
	names := make([]string, 0, len(questions))
	for n := range questions {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		q := questions[n]
		fmt.Fprintf(&b, "%s (%s): %s\n", n, q.Type, q.Instructions)
		switch crit := q.Criteria.(type) {
		case map[string]string:
			opts := make([]string, 0, len(crit))
			for o := range crit {
				opts = append(opts, o)
			}
			sort.Strings(opts)
			for _, o := range opts {
				fmt.Fprintf(&b, "  - %s: %s\n", o, crit[o])
			}
		case []string:
			for i, l := range crit {
				fmt.Fprintf(&b, "  %d: %s\n", i, l)
			}
		}
	}
	return b.String()
}

func flatten(b *strings.Builder, path string, v any) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			flatten(b, p, x[k])
		}
	case []any:
		for i, e := range x {
			flatten(b, fmt.Sprintf("%s[%d]", path, i), e)
		}
	case nil:
		fmt.Fprintf(b, "%s: null\n", path)
	default:
		fmt.Fprintf(b, "%s: %v\n", path, x)
	}
}

// guard refuses a request whose audit text matches any pattern. It never quotes the text.
func (c *Client) guard(state any, questions map[string]Question) error {
	if ms := Scan(Audit(state, questions), c.Deny); len(ms) > 0 {
		return &Error{Class: ClassSensitive, Msg: "request refused by the outbound guard: " + Describe(ms)}
	}
	return nil
}
