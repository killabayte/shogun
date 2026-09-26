package provider

import (
	"context"
	"errors"
	"testing"
)

func TestP3Round3RetryBudgetClassification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cap     int
		classes []Class
		stopped bool
	}{
		{"correction_prevented", 1, []Class{ClassPayload}, true},
		{"correction_attempted_and_failed", 2, []Class{ClassPayload, ClassPayload}, false},
		{"transport_then_correction_prevented", 2, []Class{ClassTransport, ClassPayload}, true},
		{"transport_retry_prevented", 1, []Class{ClassTransport}, true},
		{"normal_retry_ceiling", 0, []Class{ClassTransport, ClassTransport, ClassTransport}, false},
		{"config_is_terminal", 1, []Class{ClassConfig}, false},
		{"protocol_is_terminal", 1, []Class{ClassProtocol}, false},
		{"refusal_is_terminal", 1, []Class{ClassRefusal}, false},
		{"subscription_limit_is_terminal", 1, []Class{ClassRateLimit}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			result, err := runAttempts(context.Background(), Request{Dir: t.TempDir(), MaxAttempts: tc.cap},
				func(_ context.Context, _ string, _ string) (*Result, *Error) {
					calls++
					if calls > len(tc.classes) {
						t.Fatalf("unexpected attempt %d", calls)
					}
					return nil, &Error{Class: tc.classes[calls-1], Msg: "controlled failure"}
				})
			var got *Error
			if result != nil || !errors.As(err, &got) {
				t.Fatalf("result=%v error=%v; want provider error", result, err)
			}
			if calls != len(tc.classes) || got.Attempts != calls || got.Class != tc.classes[len(tc.classes)-1] || got.BudgetStopped != tc.stopped {
				t.Fatalf("calls=%d attempts=%d class=%s budgetStopped=%v; want calls=%d class=%s budgetStopped=%v",
					calls, got.Attempts, got.Class, got.BudgetStopped, len(tc.classes), tc.classes[len(tc.classes)-1], tc.stopped)
			}
		})
	}
}
