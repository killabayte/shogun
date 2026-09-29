package provider

import (
	"strings"
	"testing"
)

// No vendor key reaches a model's child process: the provider prefixes and the TYPESAFE_/JEV_
// prefixes of the closed Jev experiment are stripped; ordinary variables and extras stay.
func TestChildEnvStripsVendorKeys(t *testing.T) {
	StripFromChildren("MY_CLASSIFIER_CREDENTIAL", "")
	env := childEnv([]string{"PATH=/bin", "TYPESAFE_API_KEY=k1", "JEV_DEFAULT_TEST=k2", "ANTHROPIC_API_KEY=k3", "OPENAI_API_KEY=k4", "MY_CLASSIFIER_CREDENTIAL=k5", "HOME=/h"}, "RUST_LOG=info")
	got := strings.Join(env, " ")
	if strings.Contains(got, "k1") || strings.Contains(got, "k2") || strings.Contains(got, "k3") || strings.Contains(got, "k4") || strings.Contains(got, "k5") ||
		!strings.Contains(got, "PATH=/bin") || !strings.Contains(got, "HOME=/h") || !strings.Contains(got, "RUST_LOG=info") {
		t.Fatalf("child env %q", got)
	}
}
