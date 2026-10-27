// Package observability reports controller activity without resource identity labels.
package observability

import (
	"fmt"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is registered explicitly on the manager's registry. A nil recorder
// is safe for controllers embedded in tests or other applications.
type Metrics struct {
	rollouts            *prometheus.CounterVec
	workerActive        *prometheus.GaugeVec
	workerEvents        *prometheus.CounterVec
	workerQueue         *prometheus.HistogramVec
	historyAge          *prometheus.HistogramVec
	decisionConstraints *prometheus.CounterVec
	statusRejections    *prometheus.CounterVec
	metricEvaluations   *prometheus.CounterVec
	metricDuration      *prometheus.HistogramVec
	modelRuns           *prometheus.CounterVec
	modelDuration       *prometheus.HistogramVec
	modelEvaluations    *prometheus.CounterVec
	samplingChecks      *prometheus.CounterVec
	samplingDuration    *prometheus.HistogramVec
	decisions           *prometheus.CounterVec
	historyOperations   *prometheus.CounterVec
	historyDuration     *prometheus.HistogramVec
	historyDataBytes    *prometheus.HistogramVec
}

func New(registry prometheus.Registerer) (*Metrics, error) {
	if registry == nil {
		return nil, fmt.Errorf("missing metrics registry")
	}
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "hpaplus", Name: name, Help: help}, labels)
	}
	duration := func(name, help string, labels ...string) *prometheus.HistogramVec {
		return prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: "hpaplus", Name: name, Help: help,
			Buckets: []float64{.01, .05, .1, .5, 1, 5, 15, 30, 60, 120, 300}}, labels)
	}
	m := &Metrics{
		rollouts:            counter("rollout_evaluations_total", "Completed decision cycles by rollout mode; observe cycles never write Scale.", "mode"),
		workerActive:        prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: "hpaplus", Name: "worker_active", Help: "Current live Python processes or active exchanges; process-local."}, []string{"kind", "activity"}),
		workerEvents:        counter("worker_events_total", "Python worker lifecycle events; starts and resets expose churn without resource labels.", "kind", "event"),
		workerQueue:         duration("worker_queue_wait_seconds", "Elapsed worker acquisition or exchange-gate wait, including canceled waits.", "kind", "stage"),
		historyAge:          duration("history_sample_age_seconds", "Oldest/newest retained CPU sample age at validated model evaluation; observations, not gauges.", "family", "edge"),
		decisionConstraints: counter("decision_constraints_total", "Constraints affecting completed decisions; several may apply to one decision.", "reason"),
		statusRejections:    counter("status_budget_rejections_total", "Writes rejected by total status or pre-scale accounting budgets.", "partition"),
		metricEvaluations:   counter("metric_evaluations_total", "Metric collection/evaluation attempts by outcome, including history sampling.", "outcome"),
		metricDuration:      duration("metric_evaluation_duration_seconds", "Time spent collecting and evaluating metrics.", "outcome"),
		modelRuns:           counter("model_runs_total", "Actual model executions by family and runtime outcome, before result persistence.", "family", "outcome"),
		modelDuration:       duration("model_run_duration_seconds", "Time spent executing a model, including queue waits and hooks.", "family", "outcome"),
		modelEvaluations:    counter("model_evaluations_total", "Model eligibility assessments by final state; these are events, not current-state gauges.", "family", "state"),
		samplingChecks:      counter("sampling_checks_total", "Per-resource history sampling checks by outcome; completed may be a concurrent no-op.", "outcome"),
		samplingDuration:    duration("sampling_check_duration_seconds", "Time spent checking and sampling one resource's history.", "outcome"),
		decisions:           counter("decisions_total", "Completed and accounted scaling decisions, including holds; excludes recovery and failed writes.", "mode", "direction"),
		historyOperations:   counter("history_operations_total", "History ConfigMap I/O attempts, including conflicts and rejected writes.", "operation", "outcome"),
		historyDuration:     duration("history_operation_duration_seconds", "Time spent in one history ConfigMap I/O attempt.", "operation", "outcome"),
		historyDataBytes: prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: "hpaplus", Name: "history_configmap_data_bytes",
			Help:    "Combined data/binaryData bytes read or proposed for a history ConfigMap; observations, not a current-size gauge.",
			Buckets: []float64{1024, 4096, 16384, 65536, 131072, 262144, 524288, 786432, 1048576}}, []string{"operation"}),
	}
	collectors := []prometheus.Collector{m.rollouts, m.metricEvaluations, m.metricDuration, m.modelRuns, m.modelDuration,
		m.modelEvaluations, m.samplingChecks, m.samplingDuration, m.decisions,
		m.historyOperations, m.historyDuration, m.historyDataBytes, m.workerActive, m.workerEvents, m.workerQueue, m.historyAge, m.decisionConstraints, m.statusRejections}
	for i, collector := range collectors {
		if err := registry.Register(collector); err != nil {
			for _, registered := range collectors[:i] {
				registry.Unregister(registered)
			}
			return nil, fmt.Errorf("register HPAPlus metrics: %w", err)
		}
	}
	return m, nil
}

func category(value string, allowed ...string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return "other"
}

func family(kind string) string {
	switch kind {
	case api.TypeLinear:
		return "linear"
	case api.TypeHoltWinters:
		return "holt_winters"
	case api.TypeArima:
		return "arima"
	case api.TypeXGBoost:
		return "xgboost"
	case api.TypeLightGBM:
		return "lightgbm"
	default:
		return "other"
	}
}

func elapsedSeconds(elapsed time.Duration) float64 {
	if elapsed < 0 {
		return 0
	}
	return elapsed.Seconds()
}

func (m *Metrics) ObserveMetricEvaluation(outcome string, elapsed time.Duration) {
	if m == nil {
		return
	}
	outcome = category(outcome, "complete", "partial", "unavailable", "canceled")
	m.metricEvaluations.WithLabelValues(outcome).Inc()
	m.metricDuration.WithLabelValues(outcome).Observe(elapsedSeconds(elapsed))
}

func (m *Metrics) ObserveModelRun(kind, outcome string, elapsed time.Duration) {
	if m == nil {
		return
	}
	kind = family(kind)
	outcome = category(outcome, "success", "insufficient_history", "failed", "timeout", "canceled")
	m.modelRuns.WithLabelValues(kind, outcome).Inc()
	m.modelDuration.WithLabelValues(kind, outcome).Observe(elapsedSeconds(elapsed))
}

func (m *Metrics) ObserveModelEvaluation(kind, state string) {
	if m == nil {
		return
	}
	state = category(state, "Waiting", "WarmingUp", "Scheduled", "Ready", "Cached", "Backoff", "Failed", "InvalidHistory", "Unavailable", "Expired")
	m.modelEvaluations.WithLabelValues(family(kind), state).Inc()
}

func (m *Metrics) ObserveSampling(outcome string, elapsed time.Duration) {
	if m == nil {
		return
	}
	outcome = category(outcome, "not_due", "completed", "duplicate_source", "missing_cpu", "failed", "canceled")
	m.samplingChecks.WithLabelValues(outcome).Inc()
	m.samplingDuration.WithLabelValues(outcome).Observe(elapsedSeconds(elapsed))
}

func (m *Metrics) ObserveDecision(mode string, previous, desired int32) {
	if m == nil {
		return
	}
	mode = category(mode, "baseline", "predictive", "mixed")
	direction := "hold"
	if desired > previous {
		direction = "up"
	} else if desired < previous {
		direction = "down"
	}
	m.decisions.WithLabelValues(mode, direction).Inc()
}

func (m *Metrics) ObserveHistoryOperation(operation, outcome string, elapsed time.Duration) {
	if m == nil {
		return
	}
	operation = category(operation, "read", "write", "create")
	outcome = category(outcome, "success", "noop", "failed", "conflict", "capacity", "invalid", "ownership", "canceled")
	m.historyOperations.WithLabelValues(operation, outcome).Inc()
	m.historyDuration.WithLabelValues(operation, outcome).Observe(elapsedSeconds(elapsed))
}

func (m *Metrics) ObserveHistoryBytes(operation string, bytes int) {
	if m == nil {
		return
	}
	if bytes < 0 {
		bytes = 0
	}
	m.historyDataBytes.WithLabelValues(category(operation, "read", "write", "create")).Observe(float64(bytes))
}

func (m *Metrics) WorkerActivity(kind, activity string, delta float64) {
	if m == nil {
		return
	}
	m.workerActive.WithLabelValues(category(kind, "one_shot", "persistent"), category(activity, "processes", "exchanges")).Add(delta)
}
func (m *Metrics) ObserveWorkerEvent(kind, event string) {
	if m == nil {
		return
	}
	m.workerEvents.WithLabelValues(category(kind, "one_shot", "persistent"), category(event, "started", "exited", "failed", "reset", "limit_rejected")).Inc()
}
func (m *Metrics) ObserveWorkerQueue(kind, stage string, elapsed time.Duration) {
	if m == nil {
		return
	}
	m.workerQueue.WithLabelValues(category(kind, "one_shot", "persistent"), category(stage, "slot", "session", "exchange")).Observe(elapsedSeconds(elapsed))
}
func (m *Metrics) ObserveHistoryAge(kind, edge string, age time.Duration) {
	if m == nil {
		return
	}
	m.historyAge.WithLabelValues(family(kind), category(edge, "oldest", "newest")).Observe(elapsedSeconds(age))
}
func (m *Metrics) ObserveConstraint(reason string) {
	if m == nil {
		return
	}
	m.decisionConstraints.WithLabelValues(category(reason, "partial_metrics", "stabilization", "rate_limit", "disabled", "minimum", "maximum", "assist_baseline")).Inc()
}
func (m *Metrics) ObserveStatusRejection(partition string) {
	if m == nil {
		return
	}
	m.statusRejections.WithLabelValues(category(partition, "complete", "accounting")).Inc()
}

func (m *Metrics) ObserveRollout(mode string) {
	if m == nil {
		return
	}
	m.rollouts.WithLabelValues(category(mode, "observe", "assist", "control")).Inc()
}
