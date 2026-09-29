package provider

import (
	"strings"
	"testing"
)

// The models must not see any key: provider prefixes are stripped, and so is whatever variable the
// configuration names for Jev.
func TestChildEnvStripsProviderAndJevKeys(t *testing.T) {
	StripFromChildren("JEV_DEFAULT_TEST", "")
	env := childEnv([]string{"PATH=/bin", "TYPESAFE_API_KEY=apik1", "JEV_DEFAULT_TEST=apik2", "ANTHROPIC_API_KEY=x", "HOME=/h"}, "RUST_LOG=info")
	got := strings.Join(env, " ")
	if strings.Contains(got, "apik") || strings.Contains(got, "ANTHROPIC") || !strings.Contains(got, "PATH=/bin") || !strings.Contains(got, "HOME=/h") || !strings.Contains(got, "RUST_LOG=info") {
		t.Fatalf("child env %q", got)
	}
}
