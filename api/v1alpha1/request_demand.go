package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	PredictionFailureHoldCurrent = "HoldCurrent"
	PredictionFailureReactive    = "Reactive"

	RequestDemandTargetArrivals         = "arrivals"
	RequestDemandTargetReconstructedCPU = "reconstructedCPU"
)

// RequestDemandConfig opts an XGBoost model into arrival forecasting and online CPU-cost learning.
// Telemetry is read from the operator-configured trusted Prometheus endpoint.
type RequestDemandConfig struct {
	Traffic TrafficProviderConfig `json:"traffic"`
	// ForecastIntervalSeconds controls future applicability windows. Zero keeps
	// the existing thirty-second grid; input history remains thirty seconds.
	// +optional
	// +kubebuilder:validation:Enum=0;5;10;15;30
	ForecastIntervalSeconds int `json:"forecastIntervalSeconds,omitempty"`
	// ForecastTarget selects what is forecast. Empty or arrivals forecasts incoming
	// requests and converts them with the learned CPU cost. reconstructedCPU
	// forecasts CPU demand rebuilt from each closed window as measured CPU x
	// arrivals / completed work, so saturation no longer hides unmet demand, and
	// never recommends fewer replicas than the current reconstructed demand needs.
	// +optional
	// +kubebuilder:validation:Enum=arrivals;reconstructedCPU
	ForecastTarget string `json:"forecastTarget,omitempty"`
	// ExtraMetrics supplies optional historical inputs, not independent scaling recommendations.
	// +optional
	ExtraMetrics []DemandExtraMetric `json:"extraMetrics,omitempty"`
}

// TrafficProviderConfig identifies counters at a single observation point.
// Counter metrics must carry namespace and pod labels referring to the resolved application pods.
type TrafficProviderConfig struct {
	// +kubebuilder:validation:Enum=prometheus
	Provider string `json:"provider"`
	// +optional
	NamespaceLabel string `json:"namespaceLabel,omitempty"`
	// +optional
	PodLabel string `json:"podLabel,omitempty"`
	// PodUIDLabel defaults to pod_uid; Prometheus discovery must preserve the Kubernetes pod UID.
	// +optional
	PodUIDLabel    string `json:"podUIDLabel,omitempty"`
	ArrivalsMetric string `json:"arrivalsMetric"`
	// CompletionsMetric counts completed processing work; cheap pre-execution rejections must not be counted as equally costly completed work.
	CompletionsMetric string `json:"completionsMetric"`
	// ArrivalSemantics must be started: counters increment when a request arrives, before completion.
	// +kubebuilder:validation:Enum=started
	ArrivalSemantics string `json:"arrivalSemantics"`
	// +kubebuilder:validation:Enum=finished
	CompletionSemantics string `json:"completionSemantics"`
	// RetrySemantics documents whether retried attempts are included. No implicit deduplication is performed.
	// +kubebuilder:validation:Enum=attempts;original
	RetrySemantics string `json:"retrySemantics"`
	// AdditionalLabels restricts one observation point; reserved workload labels cannot be overridden.
	// +optional
	AdditionalLabels map[string]string `json:"additionalLabels,omitempty"`
	// +optional
	ErrorsMetric string `json:"errorsMetric,omitempty"`
	// OutcomesMetric counts all finished HTTP/gRPC outcomes, including errors, at the same observation point.
	// It supplies the denominator for the optional error fraction; CompletionsMetric remains the processed-work counter.
	// Without this counter, errors/second is available but the error fraction is unavailable.
	// +optional
	OutcomesMetric string `json:"outcomesMetric,omitempty"`
	// +optional
	LatencyBucketMetric string `json:"latencyBucketMetric,omitempty"`
	// +optional
	MaxAgeSeconds int `json:"maxAgeSeconds,omitempty"`
}

// DemandExtraMetric is a bounded gauge or counter read with the same workload scope.
type DemandExtraMetric struct {
	Name   string `json:"name"`
	Metric string `json:"metric"`
	Unit   string `json:"unit"`
	// +kubebuilder:validation:Enum=gauge;counter
	Kind string `json:"kind"`
	// +optional
	MaxAgeSeconds int `json:"maxAgeSeconds,omitempty"`
}

// RequestDemandStatus explains whether the learned conversion is qualified and which predictor is active.
type RequestDemandStatus struct {
	State                 string  `json:"state"`
	Source                string  `json:"source,omitempty"`
	FallbackReason        string  `json:"fallbackReason,omitempty"`
	CPUSecondsPerRequest  float64 `json:"cpuSecondsPerRequest,omitempty"`
	BackgroundCoresPerPod float64 `json:"backgroundCoresPerPod,omitempty"`
	QualifiedSamples      int     `json:"qualifiedSamples,omitempty"`
	Version               uint64  `json:"version,omitempty"`
	// +optional
	LastObservationAt *metav1.Time `json:"lastObservationAt,omitempty"`
	// +optional
	FreshUntil *metav1.Time `json:"freshUntil,omitempty"`

	// The fields below are reported only for forecastTarget reconstructedCPU.
	Target string `json:"target,omitempty"`
	// MeasuredCores and ReconstructedCores describe the latest closed window.
	MeasuredCores      float64 `json:"measuredCores,omitempty"`
	ReconstructedCores float64 `json:"reconstructedCores,omitempty"`
	// ReconstructionFallbacks counts history windows that used measured CPU
	// because arrival or completion counts were unavailable.
	ReconstructionFallbacks int   `json:"reconstructionFallbacks,omitempty"`
	ForecastOnlyReplicas    int32 `json:"forecastOnlyReplicas,omitempty"`
	FloorReplicas           int32 `json:"floorReplicas,omitempty"`
	// DecidedBy is forecast, floor, hold when the window that closed at the
	// last sampling boundary was not measured yet, rising when demand was
	// still increasing, fast when the last few seconds needed more pods, or
	// postponed when a new forecast took back a rise the previous one expected.
	DecidedBy string `json:"decidedBy,omitempty"`
}

func (in *RequestDemandConfig) DeepCopyInto(out *RequestDemandConfig) {
	*out = *in
	if in.ExtraMetrics != nil {
		out.ExtraMetrics = append([]DemandExtraMetric(nil), in.ExtraMetrics...)
	}
	in.Traffic.DeepCopyInto(&out.Traffic)
}
func (in *RequestDemandConfig) DeepCopy() *RequestDemandConfig {
	if in == nil {
		return nil
	}
	out := new(RequestDemandConfig)
	in.DeepCopyInto(out)
	return out
}
func (in *TrafficProviderConfig) DeepCopyInto(out *TrafficProviderConfig) {
	*out = *in
	if in.AdditionalLabels != nil {
		out.AdditionalLabels = make(map[string]string, len(in.AdditionalLabels))
		for k, v := range in.AdditionalLabels {
			out.AdditionalLabels[k] = v
		}
	}
}
func (in *RequestDemandStatus) DeepCopyInto(out *RequestDemandStatus) {
	*out = *in
	if in.LastObservationAt != nil {
		out.LastObservationAt = in.LastObservationAt.DeepCopy()
	}
	if in.FreshUntil != nil {
		out.FreshUntil = in.FreshUntil.DeepCopy()
	}
}
