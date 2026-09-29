package jev

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type pr17Transport func(*http.Request) (*http.Response, error)

func (f pr17Transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func pr17Canary() (*Client, *int) {
	calls := new(int)
	c := New("not-a-real-key")
	c.HTTP = &http.Client{Transport: pr17Transport(func(*http.Request) (*http.Response, error) {
		*calls++
		return nil, errors.New("offline canary: request reached the transport")
	})}
	return c, calls
}

func TestPR17ReviewGuardSeesLogicalText(t *testing.T) {
	for name, text := range map[string]string{
		"quoted_shell_password":    `Use PASSWORD="` + strings.Repeat("x", 12) + `"`,
		"json_password_in_task":    `Use {"password":"` + strings.Repeat("x", 12) + `"}`,
		"access_key_after_newline": "Credentials:\n" + "AKIA" + strings.Repeat("Q", 16),
		"bearer_with_newline":      "Bearer\n" + strings.Repeat("x", 16),
	} {
		t.Run(name, func(t *testing.T) {
			c, calls := pr17Canary()
			_, err := c.Ask(context.Background(), map[string]string{"task": text}, map[string]Question{"q": Noul("Evaluate the task")})
			var e *Error
			if *calls != 0 || !errors.As(err, &e) || e.Class != ClassSensitive {
				t.Fatalf("ordinary formatted sensitive text escaped the guard: transport_calls=%d error=%v", *calls, err)
			}
		})
	}
}

func TestPR17ReviewGuardCoversQuestionData(t *testing.T) {
	sample := "AKIA" + strings.Repeat("Q", 16)
	for name, q := range map[string]Question{
		"instructions": Noul("Evaluate access with " + sample),
		"criteria":     Choice("Choose", map[string]string{"yes": "use " + sample, "no": "do not use it"}),
	} {
		t.Run(name, func(t *testing.T) {
			c, calls := pr17Canary()
			_, err := c.Ask(context.Background(), map[string]string{"task": "ordinary text"}, map[string]Question{"q": q})
			var e *Error
			if *calls != 0 || !errors.As(err, &e) || e.Class != ClassSensitive {
				t.Fatalf("sensitive question data reached transport: calls=%d error=%v", *calls, err)
			}
		})
	}
}
