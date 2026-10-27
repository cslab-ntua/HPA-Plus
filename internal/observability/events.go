package observability

import (
	"sync"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
)

const eventInterval = 5 * time.Minute
const maxEventKeys = 1024

// Events is best-effort operational feedback, never scale accounting. Production
// uses client-go's asynchronous recorder, so delivery cannot gate a scale write.
type Events struct {
	recorder record.EventRecorder
	mu       sync.Mutex
	last     map[string]time.Time
	limiter  *rate.Limiter
	now      func() time.Time
}

func NewEvents(recorder record.EventRecorder) *Events {
	return &Events{recorder: recorder, last: map[string]time.Time{}, limiter: rate.NewLimiter(5, 10), now: time.Now}
}

func (e *Events) emit(instance *api.HPAPlus, reason, message string, warning bool) {
	if e == nil || e.recorder == nil || instance.UID == "" {
		return
	}
	now := e.now()
	key := string(instance.UID) + "/" + reason
	e.mu.Lock()
	if last, ok := e.last[key]; ok && now.Sub(last) < eventInterval {
		e.mu.Unlock()
		return
	}
	if !e.limiter.AllowN(now, 1) {
		e.mu.Unlock()
		return
	}
	if len(e.last) >= maxEventKeys {
		oldest := ""
		for k, stamp := range e.last {
			if oldest == "" || stamp.Before(e.last[oldest]) {
				oldest = k
			}
		}
		delete(e.last, oldest)
	}
	e.last[key] = now
	e.mu.Unlock()
	kind := corev1.EventTypeNormal
	if warning {
		kind = corev1.EventTypeWarning
	}
	e.recorder.Event(instance, kind, reason, message)
}

var conditionEvents = map[string]struct{ warning, recovery, message string }{
	"ConfigurationValid":       {"InvalidConfiguration", "ConfigurationRecovered", "Correct the HPAPlus specification; see ConfigurationValid status."},
	"MetricsAvailable":         {"MetricsUnavailable", "MetricsRecovered", "Metrics are incomplete or unavailable; inspect metric sources and MetricsAvailable status."},
	"PredictionsAvailable":     {"PredictionsUnavailable", "PredictionsRecovered", "No usable prediction is available; inspect model status, history and the configured rollout mode."},
	"SamplingActive":           {"SamplingUnavailable", "SamplingRecovered", "Sampling cannot record usable history; inspect SamplingActive status and metric/storage access."},
	"HistoryCapacityAvailable": {"HistoryCapacityLow", "HistoryCapacityRecovered", "History or journal capacity is insufficient; review retention and HistoryCapacityAvailable status."},
	"ScalingActive":            {"ScalingUnavailable", "ScalingRecovered", "Reconciliation could not complete; inspect ScalingActive status and correlated operator logs."},
	"ModelStatusComplete":      {"ModelReportsOmitted", "ModelReportsRecovered", "Detailed model reports exceed the status budget; reduce model count or inspect aggregate metrics."},
	"MetricStatusComplete":     {"MetricReportsOmitted", "MetricReportsRecovered", "Detailed metric reports exceed the status budget; inspect aggregate metrics."},
}

// Conditions runs only after a successful optimistic status patch. It compares
// the actual pre-write state, so retries and restarts do not fabricate transitions.
func (e *Events) Conditions(instance *api.HPAPlus, before, after []metav1.Condition) {
	if e == nil {
		return
	}
	for _, current := range after {
		policy, ok := conditionEvents[current.Type]
		if !ok || current.ObservedGeneration != instance.Generation {
			continue
		}
		old := meta.FindStatusCondition(before, current.Type)
		if old != nil && old.Status == current.Status && old.Reason == current.Reason && old.ObservedGeneration == current.ObservedGeneration {
			continue
		}
		if current.Status == metav1.ConditionFalse {
			// ConfigurationValid already supplies the actionable warning.
			if current.Type == "ScalingActive" && (current.Reason == "InvalidConfiguration" || current.Reason == "ObserveOnly") {
				continue
			}
			e.emit(instance, policy.warning, policy.message, true)
		} else if current.Status == metav1.ConditionTrue && old != nil && old.Status == metav1.ConditionFalse && old.Reason != "ObserveOnly" {
			e.emit(instance, policy.recovery, "The condition has recovered; see current HPAPlus status.", false)
		}
	}
}

func (e *Events) ScaleAccepted(instance *api.HPAPlus) {
	e.emit(instance, "ScaleRequested", "The workload Scale API accepted a replica change; see decision logs and status for accounting.", false)
}
func (e *Events) ScaleRecovered(instance *api.HPAPlus) {
	e.emit(instance, "ScaleAccountingRecovered", "An interrupted scale attempt was accounted conservatively; the old request was not replayed.", false)
}
func (e *Events) StatusFailed(instance *api.HPAPlus) {
	e.emit(instance, "StatusPublicationFailed", "Health status could not be published; inspect API access, status capacity and correlated operator logs.", true)
}
