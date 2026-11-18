package analysis

import (
	"slices"
	"sort"
	"strings"

	"github.com/mountain-nerd/shadowbox/internal/effect"
	"github.com/mountain-nerd/shadowbox/internal/observation"
)

func failed(o observation.Observation) bool { return o.Decision.Kind == observation.Error }

// decisionAgreement is the share of pairs (where both calls succeeded) in
// which the shadow made the same kind of decision and chose the same set of
// tools as the primary.
type decisionAgreement struct{ agree, total int }

// NewDecisionAgreement returns the decision_agreement metric.
func NewDecisionAgreement() Metric { return &decisionAgreement{} }

func (*decisionAgreement) Name() string { return MetricDecisionAgreement }

func (m *decisionAgreement) Observe(p observation.Pair) {
	if failed(p.Primary) || failed(p.Shadow) {
		return
	}
	m.total++
	a, b := p.Primary.Decision, p.Shadow.Decision
	if a.Kind == b.Kind && slices.Equal(a.ToolNames(), b.ToolNames()) {
		m.agree++
	}
}

func (m *decisionAgreement) Result() Result {
	if m.total == 0 {
		return Result{}
	}
	return Result{Value: float64(m.agree) / float64(m.total), Samples: m.total}
}

// writeToolProposals counts pairs where the shadow proposed a state-changing
// tool that the primary did not. Unknown tools count as writes.
type writeToolProposals struct {
	classifier effect.Classifier
	count      int
	total      int
	tools      map[string]struct{}
}

// NewWriteToolProposals returns the new_write_tool_proposals metric.
func NewWriteToolProposals(c effect.Classifier) Metric {
	return &writeToolProposals{classifier: c, tools: map[string]struct{}{}}
}

func (*writeToolProposals) Name() string { return MetricNewWriteToolProposals }

func (m *writeToolProposals) Observe(p observation.Pair) {
	m.total++
	primary := map[string]struct{}{}
	for _, n := range p.Primary.Decision.ToolNames() {
		primary[n] = struct{}{}
	}
	found := false
	for _, n := range p.Shadow.Decision.ToolNames() {
		if _, same := primary[n]; same || !m.classifier.Classify(n).IsWrite() {
			continue
		}
		m.tools[n] = struct{}{}
		found = true
	}
	if found {
		m.count++
	}
}

func (m *writeToolProposals) Result() Result {
	if m.total == 0 {
		return Result{}
	}
	names := make([]string, 0, len(m.tools))
	for n := range m.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return Result{Value: float64(m.count), Samples: m.total, Detail: strings.Join(names, ", ")}
}

// shadowErrorRate is the share of pairs whose shadow call failed.
type shadowErrorRate struct{ errors, total int }

// NewShadowErrorRate returns the shadow_error_rate metric.
func NewShadowErrorRate() Metric { return &shadowErrorRate{} }

func (*shadowErrorRate) Name() string { return MetricShadowErrorRate }

func (m *shadowErrorRate) Observe(p observation.Pair) {
	m.total++
	if failed(p.Shadow) {
		m.errors++
	}
}

func (m *shadowErrorRate) Result() Result {
	if m.total == 0 {
		return Result{}
	}
	return Result{Value: float64(m.errors) / float64(m.total), Samples: m.total}
}

// tokenRatio is total shadow tokens divided by total primary tokens, over
// pairs where both calls succeeded and both reported usage.
type tokenRatio struct {
	primary, shadow int
	total           int
}

// NewTokenRatio returns the token_ratio metric.
func NewTokenRatio() Metric { return &tokenRatio{} }

func (*tokenRatio) Name() string { return MetricTokenRatio }

func (m *tokenRatio) Observe(p observation.Pair) {
	if failed(p.Primary) || failed(p.Shadow) || !p.Primary.Usage.Known || !p.Shadow.Usage.Known {
		return
	}
	m.total++
	m.primary += p.Primary.Usage.Total()
	m.shadow += p.Shadow.Usage.Total()
}

func (m *tokenRatio) Result() Result {
	if m.total == 0 || m.primary == 0 {
		return Result{}
	}
	return Result{Value: float64(m.shadow) / float64(m.primary), Samples: m.total}
}
