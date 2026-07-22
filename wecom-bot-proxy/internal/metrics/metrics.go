// Package metrics holds Prometheus instruments for wecom-bot-proxy.
//
// All counters are labeled by interaction_mode (and where relevant, reason /
// kind) so operators can break down traffic and rejections per mode without
// grepping logs. The registry is pluggable via NewMetricsFor to keep tests
// hermetic.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics bundles the Prometheus instruments used by the proxy.
type Metrics struct {
	// TextInput counts inbound text messages by interaction_mode and outcome
	// kind. Outcome is one of: help_start / digit / passthrough / rejected.
	// Rejected means "dropped by the command_text digit gate"; the
	// TextInputRejected counter carries the detailed reason.
	TextInput *prometheus.CounterVec

	// TextInputRejected counts text messages rejected by the command_text
	// digit gate, broken down by reason:
	//   non_digit      — content was not 1-2 digit number, help, or start
	//   (future: too_long — currently handled by InputMaxLength upstream and
	//    counted before the gate; reserved here for forward compatibility)
	TextInputRejected *prometheus.CounterVec
}

// New returns a Metrics backed by the default prometheus registry. Calling
// this more than once per process will panic on duplicate registration — use
// NewFor in tests.
func New() *Metrics {
	return NewFor(prometheus.DefaultRegisterer)
}

// NewFor registers the metrics against the supplied registerer, allowing
// tests to use a non-global registry (prometheus.NewRegistry()) to avoid
// cross-test state leakage.
func NewFor(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		TextInput: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wecom_text_input_total",
			Help: "Total inbound text messages seen by the proxy, labeled by interaction_mode and outcome kind (help_start|digit|passthrough|rejected).",
		}, []string{"mode", "kind"}),
		TextInputRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wecom_command_text_rejected_total",
			Help: "Text messages rejected by the command_text digit gate, labeled by interaction_mode and reason (non_digit|too_long|invalid_prefix).",
		}, []string{"mode", "reason"}),
	}
	reg.MustRegister(m.TextInput)
	reg.MustRegister(m.TextInputRejected)
	return m
}

// IncTextInput is a small convenience wrapper that tolerates a nil receiver
// so call sites can omit defensive nil checks when metrics are disabled in
// tests.
func (m *Metrics) IncTextInput(mode, kind string) {
	if m == nil {
		return
	}
	if m.TextInput != nil {
		m.TextInput.WithLabelValues(mode, kind).Inc()
	}
}

// IncTextInputRejected mirrors IncTextInput for the rejection counter.
func (m *Metrics) IncTextInputRejected(mode, reason string) {
	if m == nil {
		return
	}
	if m.TextInputRejected != nil {
		m.TextInputRejected.WithLabelValues(mode, reason).Inc()
	}
}
