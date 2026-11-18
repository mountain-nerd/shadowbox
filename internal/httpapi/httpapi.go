// Package httpapi serves verdicts and accepts observations over HTTP.
//
// The verdict endpoint follows the Argo Rollouts web metric contract: a JSON
// body whose top-level "status" is "pass", "fail" or "inconclusive", meant to
// be read with jsonPath "{$.status}", successCondition `result == "pass"` and
// failureCondition `result == "fail"`. Anything else is Inconclusive in Argo,
// which pauses the rollout.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/mountain-nerd/shadowbox/internal/analysis"
	"github.com/mountain-nerd/shadowbox/internal/observation"
)

// Verdicts is the read side (implemented by experiment.Registry).
type Verdicts interface {
	Verdict(id string) (analysis.Verdict, bool)
	IDs() []string
}

// Ingestor receives observations (implemented by pairing.Pairer).
type Ingestor interface{ Add(observation.Observation) }

// Options configures the handler.
type Options struct {
	Verdicts Verdicts
	Ingestor Ingestor
	// IngestToken enables POST /v1/observations and must be sent as
	// "Authorization: Bearer <token>". Without it ingest is disabled, because
	// anyone who can post observations can influence the verdict.
	IngestToken string
	// Metrics serves /metrics when set.
	Metrics http.Handler
}

const maxBody = 1 << 20

// New returns the API handler.
func New(o Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /v1/experiments", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string][]string{"experiments": o.Verdicts.IDs()})
	})
	mux.HandleFunc("GET /v1/experiments/{id}/verdict", func(w http.ResponseWriter, r *http.Request) {
		v, ok := o.Verdicts.Verdict(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown experiment"})
			return
		}
		writeJSON(w, http.StatusOK, v)
	})
	mux.HandleFunc("POST /v1/observations", func(w http.ResponseWriter, r *http.Request) {
		if o.IngestToken == "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "ingest is disabled; configure ingestToken to enable it"})
			return
		}
		if !validToken(r.Header.Get("Authorization"), o.IngestToken) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid bearer token"})
			return
		}
		var in wireObservation
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
		if err := dec.Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		obs, err := in.toObservation()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		o.Ingestor.Add(obs)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
	})
	if o.Metrics != nil {
		mux.Handle("GET /metrics", o.Metrics)
	}
	return mux
}

func validToken(header, token string) bool {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header[len(prefix):]), []byte(token)) == 1
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// wireObservation is the ingest format. It carries no prompt text and no
// tool arguments, only an argument hash.
type wireObservation struct {
	RequestID   string `json:"requestId"`
	Traceparent string `json:"traceparent"`
	Experiment  string `json:"experiment"`
	Role        string `json:"role"`
	Model       string `json:"model"`
	Status      int    `json:"status"`
	Decision    struct {
		Kind      string `json:"kind"`
		ToolCalls []struct {
			Name     string `json:"name"`
			ArgsHash string `json:"argsHash"`
		} `json:"toolCalls"`
	} `json:"decision"`
	Usage *struct {
		InputTokens  int `json:"inputTokens"`
		OutputTokens int `json:"outputTokens"`
	} `json:"usage"`
}

func (in wireObservation) toObservation() (observation.Observation, error) {
	if in.RequestID == "" {
		return observation.Observation{}, errors.New("requestId is required")
	}
	o := observation.Observation{
		RequestID:  in.RequestID,
		TraceID:    observation.TraceIDFromTraceparent(in.Traceparent),
		Experiment: in.Experiment,
		Model:      in.Model,
		Status:     in.Status,
		At:         time.Now(),
	}
	switch in.Role {
	case "primary":
		o.Role = observation.Primary
	case "shadow":
		o.Role = observation.Shadow
	default:
		return observation.Observation{}, errors.New(`role must be "primary" or "shadow"`)
	}
	switch in.Decision.Kind {
	case "text":
		o.Decision.Kind = observation.Text
	case "tool_calls":
		o.Decision.Kind = observation.ToolCalls
	case "error":
		o.Decision.Kind = observation.Error
	default:
		return observation.Observation{}, errors.New(`decision.kind must be "text", "tool_calls" or "error"`)
	}
	for _, c := range in.Decision.ToolCalls {
		o.Decision.ToolCalls = append(o.Decision.ToolCalls, observation.ToolCall{Name: c.Name, ArgsHash: c.ArgsHash})
	}
	if in.Usage != nil {
		o.Usage = observation.Usage{InputTokens: in.Usage.InputTokens, OutputTokens: in.Usage.OutputTokens, Known: true}
	}
	return o, nil
}
