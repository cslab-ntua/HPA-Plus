package v1alpha1

// ForecastReliabilityPolicy opts into timestamp-matched forecast error monitoring.
// It never supplies replicas or changes prediction expiry. Omitted fields use
// 20 calibration errors, a five-error window, four breaches and a 10% floor.
type ForecastReliabilityPolicy struct {
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=100
	// +optional
	CalibrationSamples int `json:"calibrationSamples,omitempty"`
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=20
	// +optional
	Window int `json:"window,omitempty"`
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=20
	// +optional
	Breaches int `json:"breaches,omitempty"`
	// +kubebuilder:validation:Minimum=0.001
	// +kubebuilder:validation:Maximum=1
	// +optional
	MinimumRelativeError float64 `json:"minimumRelativeError,omitempty"`
}

func (p ForecastReliabilityPolicy) Effective() ForecastReliabilityPolicy {
	if p.CalibrationSamples == 0 {
		p.CalibrationSamples = 20
	}
	if p.Window == 0 {
		p.Window = 5
	}
	if p.Breaches == 0 {
		p.Breaches = 4
	}
	if p.MinimumRelativeError == 0 {
		p.MinimumRelativeError = .1
	}
	return p
}

// ForecastVerification is an unmodified raw-demand prediction for one future
// observation timestamp. Exact strings retain nanoseconds across restarts.
// Until is the exclusive matching deadline; no interpolation is performed.
type ForecastVerification struct {
	At               string `json:"at"`
	Until            string `json:"until"`
	IssuedAt         string `json:"issuedAt"`
	CPU              int64  `json:"cpu"`
	LeadMilliseconds int64  `json:"leadMilliseconds"`
}

// ForecastReliabilityState is bounded internal history, isolated by the same
// fingerprint as forecasts (resource, target, model, history epoch, capacity).
type ForecastReliabilityState struct {
	Fingerprint         string                 `json:"fingerprint"`
	LeadMilliseconds    int64                  `json:"leadMilliseconds"`
	Pending             []ForecastVerification `json:"pending,omitempty"`
	Baseline            []float64              `json:"baseline,omitempty"`
	Recent              []float64              `json:"recent,omitempty"`
	LastObservationTime string                 `json:"lastObservationTime,omitempty"`
	LastEvaluationTime  string                 `json:"lastEvaluationTime,omitempty"`
	LastIntervalEnd     string                 `json:"lastIntervalEnd,omitempty"`
	ErrorBound          float64                `json:"errorBound"`
	Degraded            bool                   `json:"degraded"`
	AdaptationPending   bool                   `json:"adaptationPending"`
	Adaptation          string                 `json:"adaptation,omitempty"`
}

// ForecastReliabilityStatus reports diagnostic reliability independently of
// forecast eligibility. A degraded forecast is not replaced by reactive demand.
type ForecastReliabilityStatus struct {
	State              string  `json:"state"`
	CalibrationSamples int     `json:"calibrationSamples"`
	RecentSamples      int     `json:"recentSamples"`
	Underpredictions   int     `json:"underpredictions"`
	Overpredictions    int     `json:"overpredictions"`
	ErrorBound         float64 `json:"errorBound"`
	AdaptationPending  bool    `json:"adaptationPending"`
	Adaptation         string  `json:"adaptation,omitempty"`
}
