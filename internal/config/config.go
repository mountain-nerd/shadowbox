// Package config loads and validates shadowbox's YAML configuration and
// builds the domain objects from it (the composition root's input).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/mountain-nerd/shadowbox/internal/analysis"
	"github.com/mountain-nerd/shadowbox/internal/effect"
	"github.com/mountain-nerd/shadowbox/internal/experiment"
	"github.com/mountain-nerd/shadowbox/internal/extproc"
	"github.com/mountain-nerd/shadowbox/internal/llm"
	"github.com/mountain-nerd/shadowbox/internal/llm/openai"
)

// Listen holds listen addresses.
type Listen struct {
	HTTP string
	GRPC string
}

// Pairing configures the pairer.
type Pairing struct {
	TTL        time.Duration
	MaxPending int
}

// MCPServer is an MCP server whose tool annotations seed the effect registry.
type MCPServer struct {
	URL     string
	Trusted bool
}

// Candidate describes how the candidate differs from the primary.
type Candidate struct {
	Model        string
	SystemPrompt string
	stripTools   bool
	dropState    bool
}

// StripProviderTools reports whether provider-executed tools are removed (default true).
func (c Candidate) StripProviderTools() bool { return c.stripTools }

// DropStatefulFields reports whether store/previous_response_id are removed (default true).
func (c Candidate) DropStatefulFields() bool { return c.dropState }

// Experiment is one candidate under evaluation.
type Experiment struct {
	ID        string
	Candidate Candidate
	Gate      analysis.Gate
}

// Config is the validated configuration.
type Config struct {
	Listen       Listen
	Pairing      Pairing
	IngestToken  string
	ExtProcToken string
	Tools        map[string]effect.Class
	MCP          []MCPServer
	Experiments  []Experiment
}

// DefaultGate is used when an experiment configures no gate.
func DefaultGate() analysis.Gate {
	return analysis.Gate{MinPairs: 20, Rules: []analysis.Rule{
		{Metric: analysis.MetricNewWriteToolProposals, Op: analysis.AtMost, Threshold: 0, FailFast: true},
		{Metric: analysis.MetricDecisionAgreement, Op: analysis.AtLeast, Threshold: 0.8},
		{Metric: analysis.MetricShadowErrorRate, Op: analysis.AtMost, Threshold: 0.05},
		{Metric: analysis.MetricTokenRatio, Op: analysis.AtMost, Threshold: 1.5},
	}}
}

var knownMetrics = map[string]bool{
	analysis.MetricDecisionAgreement:     true,
	analysis.MetricNewWriteToolProposals: true,
	analysis.MetricShadowErrorRate:       true,
	analysis.MetricTokenRatio:            true,
	experiment.MetricUnpairedShadows:     true,
}

// raw is the on-disk schema.
type raw struct {
	Listen struct {
		HTTP string `json:"http"`
		GRPC string `json:"grpc"`
	} `json:"listen"`
	Pairing struct {
		TTL        string `json:"ttl"`
		MaxPending int    `json:"maxPending"`
	} `json:"pairing"`
	IngestToken  string            `json:"ingestToken"`
	ExtProcToken string            `json:"extprocToken"`
	Tools        map[string]string `json:"tools"`
	MCP          struct {
		Servers []struct {
			URL     string `json:"url"`
			Trusted bool   `json:"trusted"`
		} `json:"servers"`
	} `json:"mcp"`
	Experiments []struct {
		ID        string `json:"id"`
		Candidate struct {
			Model              string `json:"model"`
			SystemPrompt       string `json:"systemPrompt"`
			SystemPromptFile   string `json:"systemPromptFile"`
			StripProviderTools *bool  `json:"stripProviderTools"`
			DropStatefulFields *bool  `json:"dropStatefulFields"`
		} `json:"candidate"`
		Gate *struct {
			MinPairs *int `json:"minPairs"`
			Rules    []struct {
				Metric    string  `json:"metric"`
				Op        string  `json:"op"`
				Threshold float64 `json:"threshold"`
				FailFast  bool    `json:"failFast"`
			} `json:"rules"`
		} `json:"gate"`
	} `json:"experiments"`
}

// Load reads, defaults and validates a configuration file. Relative
// systemPromptFile paths are resolved against the config file's directory.
// SHADOWBOX_INGEST_TOKEN overrides ingestToken and SHADOWBOX_EXTPROC_TOKEN
// overrides extprocToken, which is required.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r raw
	if err := yaml.UnmarshalStrict(data, &r); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c := &Config{
		Listen:      Listen{HTTP: or(r.Listen.HTTP, ":8080"), GRPC: or(r.Listen.GRPC, ":9000")},
		Pairing:     Pairing{TTL: 30 * time.Second, MaxPending: 10000},
		IngestToken: r.IngestToken,
		Tools:       map[string]effect.Class{},
	}
	if tok := os.Getenv("SHADOWBOX_INGEST_TOKEN"); tok != "" {
		c.IngestToken = tok
	}
	c.ExtProcToken = r.ExtProcToken
	if tok := os.Getenv("SHADOWBOX_EXTPROC_TOKEN"); tok != "" {
		c.ExtProcToken = tok
	}
	if c.ExtProcToken == "" {
		return nil, errors.New("config: extprocToken (or SHADOWBOX_EXTPROC_TOKEN) is required: every Envoy ext_proc stream must present it")
	}
	for name, tok := range map[string]string{"extprocToken": c.ExtProcToken, "ingestToken": c.IngestToken} {
		if isPlaceholder(tok) {
			return nil, fmt.Errorf("config: %s is still the example placeholder; generate a random secret", name)
		}
	}
	if r.Pairing.TTL != "" {
		if c.Pairing.TTL, err = time.ParseDuration(r.Pairing.TTL); err != nil || c.Pairing.TTL <= 0 {
			return nil, fmt.Errorf("config: pairing.ttl %q is not a positive duration", r.Pairing.TTL)
		}
	}
	if r.Pairing.MaxPending > 0 {
		c.Pairing.MaxPending = r.Pairing.MaxPending
	}
	for tool, s := range r.Tools {
		cls, err := effect.ParseClass(s)
		if err != nil {
			return nil, fmt.Errorf("config: tools.%s: %w", tool, err)
		}
		c.Tools[tool] = cls
	}
	for _, s := range r.MCP.Servers {
		if s.URL == "" {
			return nil, errors.New("config: mcp.servers[].url is required")
		}
		c.MCP = append(c.MCP, MCPServer{URL: s.URL, Trusted: s.Trusted})
	}
	if len(r.Experiments) == 0 {
		return nil, errors.New("config: at least one experiment is required")
	}
	seen := map[string]bool{}
	for i, re := range r.Experiments {
		if re.ID == "" {
			return nil, fmt.Errorf("config: experiments[%d].id is required", i)
		}
		if seen[re.ID] {
			return nil, fmt.Errorf("config: duplicate experiment id %q", re.ID)
		}
		seen[re.ID] = true
		e := Experiment{ID: re.ID, Gate: DefaultGate()}
		rc := re.Candidate
		if rc.SystemPrompt != "" && rc.SystemPromptFile != "" {
			return nil, fmt.Errorf("config: experiment %q: set systemPrompt or systemPromptFile, not both", re.ID)
		}
		e.Candidate = Candidate{Model: rc.Model, SystemPrompt: rc.SystemPrompt, stripTools: boolOr(rc.StripProviderTools, true), dropState: boolOr(rc.DropStatefulFields, true)}
		if rc.SystemPromptFile != "" {
			p := rc.SystemPromptFile
			if !filepath.IsAbs(p) {
				p = filepath.Join(filepath.Dir(path), p)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, fmt.Errorf("config: experiment %q: %w", re.ID, err)
			}
			e.Candidate.SystemPrompt = strings.TrimSpace(string(b))
		}
		if e.Candidate.Model == "" && e.Candidate.SystemPrompt == "" {
			return nil, fmt.Errorf("config: experiment %q: candidate must set a model and/or a system prompt", re.ID)
		}
		if re.Gate != nil {
			if re.Gate.MinPairs != nil {
				e.Gate.MinPairs = *re.Gate.MinPairs
			}
			if len(re.Gate.Rules) > 0 {
				e.Gate.Rules = nil
			}
			for _, rr := range re.Gate.Rules {
				if !knownMetrics[rr.Metric] {
					return nil, fmt.Errorf("config: experiment %q: unknown metric %q", re.ID, rr.Metric)
				}
				op, err := analysis.ParseOp(rr.Op)
				if err != nil {
					return nil, fmt.Errorf("config: experiment %q: %w", re.ID, err)
				}
				e.Gate.Rules = append(e.Gate.Rules, analysis.Rule{Metric: rr.Metric, Op: op, Threshold: rr.Threshold, FailFast: rr.FailFast})
			}
		}
		c.Experiments = append(c.Experiments, e)
	}
	return c, nil
}

// EffectRegistry builds the operator's tool registry.
func (c *Config) EffectRegistry() *effect.Registry { return effect.NewRegistry(c.Tools) }

// Build creates an analyzer and a shadow rewrite for each experiment.
func (c *Config) Build(reg effect.Classifier) (map[string]*experiment.Analyzer, map[string]extproc.Experiment) {
	analyzers := make(map[string]*experiment.Analyzer, len(c.Experiments))
	rewrites := make(map[string]extproc.Experiment, len(c.Experiments))
	for _, e := range c.Experiments {
		analyzers[e.ID] = experiment.NewAnalyzer(e.Gate,
			analysis.NewDecisionAgreement(),
			analysis.NewWriteToolProposals(reg),
			analysis.NewShadowErrorRate(),
			analysis.NewTokenRatio(),
		)
		var p llm.Pipeline
		if e.Candidate.Model != "" {
			p = append(p, openai.ModelOverride{Model: e.Candidate.Model})
		}
		if e.Candidate.SystemPrompt != "" {
			p = append(p, openai.SystemPrompt{Text: e.Candidate.SystemPrompt})
		}
		if e.Candidate.StripProviderTools() {
			p = append(p, openai.StripProviderTools{})
		}
		allow := openai.AllowFields{}
		if e.Candidate.DropStatefulFields() {
			p = append(p, openai.DropStatefulFields{})
		} else {
			allow.Extra = []string{"store", "previous_response_id"}
		}
		// Last, so it sees the final request: anything not known to be
		// side-effect free is refused and the shadow copy fails closed.
		p = append(p, allow)
		rewrites[e.ID] = extproc.Experiment{Model: e.Candidate.Model, Rewrite: p}
	}
	return analyzers, rewrites
}

// isPlaceholder reports whether tok is the example manifests' placeholder,
// which is public and so no secret at all.
func isPlaceholder(tok string) bool {
	return strings.HasPrefix(strings.ToUpper(tok), "REPLACE-WITH")
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func boolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}
