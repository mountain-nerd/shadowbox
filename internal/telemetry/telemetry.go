// Package telemetry exports shadowbox's state as OpenTelemetry metrics,
// served in Prometheus format through the OTel Prometheus exporter.
//
// All instruments are observable: they read the current state on collection,
// so there is no bookkeeping on the hot path. Attributes are restricted to
// experiment id, metric name, verdict status and a fixed set of reasons;
// request and trace ids never become labels.
package telemetry

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/mountain-nerd/shadowbox/internal/analysis"
	"github.com/mountain-nerd/shadowbox/internal/experiment"
	"github.com/mountain-nerd/shadowbox/internal/extproc"
	"github.com/mountain-nerd/shadowbox/internal/pairing"
)

// Registry is the read side of experiment.Registry.
type Registry interface {
	IDs() []string
	Verdict(string) (analysis.Verdict, bool)
	Stats() experiment.Stats
}

// Sources are read on every collection.
type Sources struct {
	Pairing  func() pairing.Stats
	Registry Registry
	Shadow   func() extproc.Stats
}

var statuses = []analysis.Status{analysis.Pass, analysis.Fail, analysis.Inconclusive}

// NewHandler returns a /metrics handler and a shutdown function.
func NewHandler(s Sources) (http.Handler, func(context.Context) error, error) {
	reg := prometheus.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(reg), otelprom.WithoutTargetInfo())
	if err != nil {
		return nil, nil, err
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	m := mp.Meter("github.com/mountain-nerd/shadowbox")

	pairs, err1 := m.Int64ObservableCounter("shadowbox.pairs", metric.WithDescription("Primary/shadow pairs formed."))
	pending, err2 := m.Int64ObservableGauge("shadowbox.pending", metric.WithDescription("Observations waiting for their counterpart."))
	unpaired, err3 := m.Int64ObservableCounter("shadowbox.unpaired", metric.WithDescription("Observations that were never paired, by reason."))
	rejected, err4 := m.Int64ObservableCounter("shadowbox.shadow.rejected", metric.WithDescription("Shadow requests refused (fail-closed), by reason."))
	verdict, err5 := m.Int64ObservableGauge("shadowbox.verdict", metric.WithDescription("1 for an experiment's current verdict status, 0 otherwise."))
	expPairs, err6 := m.Int64ObservableGauge("shadowbox.experiment.pairs", metric.WithDescription("Pairs analyzed per experiment."))
	expMetric, err7 := m.Float64ObservableGauge("shadowbox.experiment.metric", metric.WithDescription("Current value of each analysis metric per experiment."))
	for _, e := range []error{err1, err2, err3, err4, err5, err6, err7} {
		if e != nil {
			return nil, nil, e
		}
	}

	reason := func(r string) metric.ObserveOption { return metric.WithAttributes(attribute.String("reason", r)) }
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		ps := s.Pairing()
		o.ObserveInt64(pairs, ps.Paired)
		o.ObserveInt64(pending, ps.Pending)
		o.ObserveInt64(unpaired, ps.Expired, reason("expired"))
		o.ObserveInt64(unpaired, ps.Evicted, reason("evicted"))
		o.ObserveInt64(unpaired, ps.Dropped, reason("dropped"))
		rs := s.Registry.Stats()
		o.ObserveInt64(unpaired, rs.UnsampledPrimaries, reason("unsampled_primary"))
		o.ObserveInt64(unpaired, rs.UnknownExperiment, reason("unknown_experiment"))
		ss := s.Shadow()
		o.ObserveInt64(rejected, ss.Rejected, reason("unknown_experiment"))
		o.ObserveInt64(rejected, ss.RewriteFailures, reason("rewrite_failure"))

		for _, id := range s.Registry.IDs() {
			v, ok := s.Registry.Verdict(id)
			if !ok {
				continue
			}
			exp := attribute.String("experiment", id)
			for _, st := range statuses {
				val := int64(0)
				if v.Status == st {
					val = 1
				}
				o.ObserveInt64(verdict, val, metric.WithAttributes(exp, attribute.String("status", string(st))))
			}
			o.ObserveInt64(expPairs, int64(v.Pairs), metric.WithAttributes(exp))
			for name, r := range v.Metrics {
				if r.Samples > 0 {
					o.ObserveFloat64(expMetric, r.Value, metric.WithAttributes(exp, attribute.String("metric", name)))
				}
			}
		}
		return nil
	}, pairs, pending, unpaired, rejected, verdict, expPairs, expMetric)
	if err != nil {
		return nil, nil, err
	}
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), mp.Shutdown, nil
}
