// Package analysis computes agent-specific rollout metrics from paired
// primary/shadow calls (Strategy: Metric) and turns them into a verdict
// (Specification: Rule, Gate).
package analysis

import "github.com/mountain-nerd/shadowbox/internal/observation"

// Metric names used in configuration and in the verdict.
const (
	MetricDecisionAgreement     = "decision_agreement"
	MetricNewWriteToolProposals = "new_write_tool_proposals"
	MetricShadowErrorRate       = "shadow_error_rate"
	MetricTokenRatio            = "token_ratio"
)

// Result is a metric's current value. Samples is the number of pairs the
// value is based on; zero means "no data".
type Result struct {
	Value   float64 `json:"value"`
	Samples int     `json:"samples"`
	Detail  string  `json:"detail,omitempty"`
}

// Metric accumulates pairs. Implementations are not safe for concurrent use;
// the caller serializes access.
type Metric interface {
	Name() string
	Observe(observation.Pair)
	Result() Result
}
