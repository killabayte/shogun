// Package config loads Shogun configuration from layered TOML files and flags,
// parses model specs and enforces the fixed roles and the reviewer effort floor.
package config

import (
	"fmt"
	"strings"
)

// Effort is a reasoning effort level accepted by both CLIs.
type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

var effortRank = map[Effort]int{EffortLow: 0, EffortMedium: 1, EffortHigh: 2, EffortXHigh: 3, EffortMax: 4}

// ReviewerMinEffort is hardcoded: the reviewer never runs below high.
const ReviewerMinEffort = EffortHigh

// Providers are fixed per role in v1.
const (
	ProviderClaude = "claude"
	ProviderCodex  = "codex"
)

// ModelSpec is "provider/model:effort". The model id is an opaque string.
type ModelSpec struct {
	Provider string
	Model    string
	Effort   Effort
}

// ParseModelSpec parses "provider/model:effort". All three parts are required.
func ParseModelSpec(s string) (ModelSpec, error) {
	s = strings.TrimSpace(s)
	slash := strings.IndexByte(s, '/')
	if slash <= 0 {
		return ModelSpec{}, fmt.Errorf("model spec %q: expected provider/model:effort", s)
	}
	provider := s[:slash]
	rest := s[slash+1:]
	colon := strings.LastIndexByte(rest, ':')
	if colon <= 0 || colon == len(rest)-1 {
		return ModelSpec{}, fmt.Errorf("model spec %q: effort is required (provider/model:effort)", s)
	}
	model, effort := rest[:colon], Effort(rest[colon+1:])
	if provider != ProviderClaude && provider != ProviderCodex {
		return ModelSpec{}, fmt.Errorf("model spec %q: unknown provider %q (claude|codex)", s, provider)
	}
	if strings.ContainsAny(model, " \t/\\\"'") {
		return ModelSpec{}, fmt.Errorf("model spec %q: invalid model id %q", s, model)
	}
	if _, ok := effortRank[effort]; !ok {
		return ModelSpec{}, fmt.Errorf("model spec %q: unknown effort %q (low|medium|high|xhigh|max)", s, effort)
	}
	return ModelSpec{Provider: provider, Model: model, Effort: effort}, nil
}

// String renders the canonical form.
func (m ModelSpec) String() string { return m.Provider + "/" + m.Model + ":" + string(m.Effort) }

// EffortAtLeast reports whether e >= min.
func EffortAtLeast(e, min Effort) bool { return effortRank[e] >= effortRank[min] }
