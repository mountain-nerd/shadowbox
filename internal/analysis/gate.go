package analysis

import (
	"fmt"
	"strings"
)

// Op compares a metric value with a threshold. Both comparisons are inclusive.
type Op int

const (
	// AtMost passes when value <= threshold.
	AtMost Op = iota + 1
	// AtLeast passes when value >= threshold.
	AtLeast
)

func (o Op) holds(value, threshold float64) bool {
	if o == AtLeast {
		return value >= threshold
	}
	return value <= threshold
}

func (o Op) String() string {
	if o == AtLeast {
		return ">="
	}
	return "<="
}

// ParseOp parses "at_most" or "at_least".
func ParseOp(s string) (Op, error) {
	switch strings.ToLower(s) {
	case "at_most", "<=":
		return AtMost, nil
	case "at_least", ">=":
		return AtLeast, nil
	}
	return 0, fmt.Errorf("analysis: unknown op %q (want at_most or at_least)", s)
}

// Rule is one requirement on one metric. A FailFast rule fails the verdict as
// soon as it is violated, even before the gate has MinPairs pairs; use it for
// counts where a single occurrence is disqualifying.
type Rule struct {
	Metric    string  `json:"metric"`
	Op        Op      `json:"-"`
	Threshold float64 `json:"threshold"`
	FailFast  bool    `json:"failFast,omitempty"`
}

// Status is the verdict status. The string values are the Argo Rollouts
// contract: successCondition result == "pass", failureCondition result == "fail";
// anything else is Inconclusive and pauses the rollout.
type Status string

const (
	Pass         Status = "pass"
	Fail         Status = "fail"
	Inconclusive Status = "inconclusive"
)

// Verdict is the gate's decision.
type Verdict struct {
	Status  Status            `json:"status"`
	Pairs   int               `json:"pairs"`
	Reasons []string          `json:"reasons"`
	Metrics map[string]Result `json:"metrics"`
}

// Gate turns metric results into a verdict. It never passes without data:
// fewer than MinPairs pairs, a missing metric or a metric without samples is
// inconclusive. Any violated rule (subject to FailFast and MinPairs) fails.
type Gate struct {
	MinPairs int
	Rules    []Rule
}

// Evaluate decides the verdict for the given number of pairs and results.
func (g Gate) Evaluate(pairs int, results map[string]Result) Verdict {
	v := Verdict{Pairs: pairs, Metrics: results, Reasons: []string{}}
	enough := pairs > 0 && pairs >= g.MinPairs
	var violations, gaps []string

	if !enough {
		gaps = append(gaps, fmt.Sprintf("only %d of %d required pairs", pairs, g.MinPairs))
	}
	for _, r := range g.Rules {
		res, ok := results[r.Metric]
		switch {
		case !ok:
			gaps = append(gaps, fmt.Sprintf("%s: metric not available", r.Metric))
		case res.Samples == 0:
			gaps = append(gaps, fmt.Sprintf("%s: no samples yet", r.Metric))
		case !r.Op.holds(res.Value, r.Threshold) && (enough || r.FailFast):
			msg := fmt.Sprintf("%s = %.4g, want %s %.4g", r.Metric, res.Value, r.Op, r.Threshold)
			if res.Detail != "" {
				msg += " (" + res.Detail + ")"
			}
			violations = append(violations, msg)
		}
	}

	switch {
	case len(violations) > 0:
		v.Status, v.Reasons = Fail, violations
	case len(gaps) > 0:
		v.Status, v.Reasons = Inconclusive, gaps
	default:
		v.Status = Pass
	}
	return v
}
