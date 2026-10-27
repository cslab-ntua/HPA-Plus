/*
Copyright 2023 The Predictive Horizontal Pod Autoscaler Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// DecisionMaximum means use the highest predicted value from the models
	DecisionMaximum = "maximum"
	// DecisionMinimum means use the lowest predicted value from the models
	DecisionMinimum = "minimum"
	// DecisionMean means use the mean average of predicted values
	DecisionMean = "mean"
	// DecisionMedian means use the median average of predicted values
	DecisionMedian = "median"
)

const (
	TypeHoltWinters = "HoltWinters"
	TypeLinear      = "Linear"
	TypeArima       = "ARIMA"
	TypeXGBoost     = "XGBoost"
	TypeLightGBM    = "LightGBM"
)

const (
	HookTypeHTTP = "http"
)

// HookDefinition describes a hook for passing data/triggering logic, such as through a shell command
type HookDefinition struct {
	// +kubebuilder:validation:Enum=http
	Type string `json:"type"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=9223372036854
	Timeout int `json:"timeout"`
	// +optional
	HTTP *HTTPHook `json:"http"`
}

// HTTPHook describes configuration options for an HTTP request hook
type HTTPHook struct {
	// +kubebuilder:validation:Enum=GET;HEAD;POST;PUT;DELETE;CONNECT;OPTIONS;TRACE;PATCH
	Method string `json:"method"`
	// +kubebuilder:validation:MinLength=1
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Minimum=100
	// +kubebuilder:validation:items:Maximum=599
	SuccessCodes []int `json:"successCodes"`
	// +kubebuilder:validation:Enum=query;body
	ParameterMode string `json:"parameterMode"`
}

// Linear represents a linear regression prediction model configuration
type Linear struct {
	// historySize is how many timestamped replica counts should be stored for this linear regression, with older
	// timestamped replica counts being removed from the data as new ones are added. For example a value of 6 means
	// there will only be a maxmimu of 6 stored timestamped replica counts for this model.
	// +kubebuilder:validation:Minimum=2
	HistorySize int `json:"historySize"`
	// lookAhead is how far in the future the linear regression should predict in milliseconds. For example a value of
	// 10000 will predict 10 seconds into the future
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=9223372036854
	LookAhead int `json:"lookAhead"`
}

// HoltWinters represents a holt-winters exponential smoothing prediction model configuration
type HoltWinters struct {
	// lookAhead is the horizon in milliseconds after the newest CPU sample.
	// Omission preserves a single forecast step at the median observed interval.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=9223372036854
	LookAhead *int `json:"lookAhead,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +optional
	Alpha *float64 `json:"alpha"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +optional
	Beta *float64 `json:"beta"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +optional
	Gamma *float64 `json:"gamma"`

	// +kubebuilder:validation:Enum=add;additive;mul;multiplicative
	Trend string `json:"trend"`

	// +kubebuilder:validation:Enum=add;additive;mul;multiplicative
	Seasonal string `json:"seasonal"`

	// +kubebuilder:validation:Minimum=2
	SeasonalPeriods int `json:"seasonalPeriods"`

	// +kubebuilder:validation:Minimum=1
	StoredSeasons int `json:"storedSeasons"`

	// +optional
	DampedTrend *bool `json:"dampedTrend"`

	// +optional
	// +kubebuilder:validation:Enum=estimated;heuristic;known;legacy-heuristic
	InitializationMethod *string `json:"initializationMethod"`

	// +optional
	InitialLevel *float64 `json:"initialLevel"`

	// +optional
	InitialTrend *float64 `json:"initialTrend"`

	// +optional
	InitialSeasonal *float64 `json:"initialSeasonal"`

	// +optional
	RuntimeTuningFetchHook *HookDefinition `json:"runtimeTuningFetchHook"`
}

// XGBoost represents a gradient boosted regression tree configuration driven by XGBoost.
// The runtime predictor trains on aggregate CPU usage history and converts the predicted
// CPU demand back into replicas using the current pod CPU request and target utilization.
type XGBoost struct {
	// useUtilizationFeatures controls optional usage/request lag features.
	// Omitted or true preserves existing behavior. False trains on aggregate CPU
	// without making replica-dependent utilization an input to the forecaster.
	// CPU history, scaling targets and replica conversion remain unchanged.
	// +optional
	UseUtilizationFeatures *bool `json:"useUtilizationFeatures,omitempty"`

	// seasonalFeatures adds target-time phase features for a cycle inferred only
	// from the retained CPU observations. Unreliable cycles leave these features missing.
	// +optional
	SeasonalFeatures bool `json:"seasonalFeatures,omitempty"`

	// forecastAggregation selects the final recursive forecast (last, the default)
	// or the maximum predicted CPU demand across the look-ahead steps (maximum).
	// maximum uses predictions from this single model, not the reactive baseline.
	// +kubebuilder:validation:Enum=last;maximum
	// +optional
	ForecastAggregation string `json:"forecastAggregation,omitempty"`

	// forecastStrategy defaults to recursive. direct trains one XGBoost with a
	// lead-time feature on matched historical targets, without feeding predictions
	// back into lag features. Missing targets are omitted, not interpolated.
	// +kubebuilder:validation:Enum=recursive;direct
	// +optional
	ForecastStrategy string `json:"forecastStrategy,omitempty"`

	// historySize is how many timestamped replica counts should be stored for this model, older entries are pruned.
	// +kubebuilder:validation:Minimum=1
	HistorySize int `json:"historySize"`

	// lookAhead is how far ahead (in milliseconds) the prediction should forecast.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=9223372036854
	LookAhead int `json:"lookAhead"`

	// lags controls how many previous CPU usage samples are used as features for training.
	// +kubebuilder:validation:Minimum=1
	Lags int `json:"lags"`

	// +kubebuilder:validation:Minimum=1
	// +optional
	WindowSize *int `json:"windowSize,omitempty"`

	// maxDepth is the maximum tree depth; HPA+ requires a positive value for XGBoost.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxDepth *int `json:"maxDepth,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +optional
	NEstimators *int `json:"nEstimators,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:ExclusiveMinimum=true
	// +optional
	LearningRate *float64 `json:"learningRate,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:ExclusiveMinimum=true
	// +kubebuilder:validation:Maximum=1
	// +optional
	Subsample *float64 `json:"subsample,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:ExclusiveMinimum=true
	// +kubebuilder:validation:Maximum=1
	// +optional
	ColsampleBytree *float64 `json:"colsampleBytree,omitempty"`

	// objective defines the XGBoost objective function, defaults to reg:squarederror.
	// +optional
	Objective *string `json:"objective,omitempty"`

	// quantileAlpha selects one CPU-demand quantile for reg:quantileerror.
	// It must be supplied only with that objective. Other objectives keep their defaults.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:ExclusiveMinimum=true
	// +kubebuilder:validation:Maximum=1
	// +kubebuilder:validation:ExclusiveMaximum=true
	// +optional
	QuantileAlpha *float64 `json:"quantileAlpha,omitempty"`

	// cpuDemandWeightExponent optionally weights observed training targets by
	// their CPU demand relative to the median, clipped to [0.5, 4]. Omitted or
	// zero preserves unweighted training. This changes loss weights, not samples.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=2
	// +optional
	CPUDemandWeightExponent *float64 `json:"cpuDemandWeightExponent,omitempty"`

	// aftLossDistributionScale configures normal log-demand noise for survival:aft.
	// Required only with that objective, which requires direct forecasting.
	// +kubebuilder:validation:Minimum=0.05
	// +kubebuilder:validation:Maximum=2
	// +optional
	AFTLossDistributionScale *float64 `json:"aftLossDistributionScale,omitempty"`

	// cpuCensoringUtilizationThreshold explicitly identifies CPU saturation,
	// expressed as usage/request percent. Required only for survival:aft.
	// This is a workload-specific assertion about enforced CPU capacity, not
	// automatically inferred from requests. Use 90 only when limits equal requests.
	// +kubebuilder:validation:Minimum=90
	// +kubebuilder:validation:Maximum=10000
	// +optional
	CPUCensoringUtilizationThreshold *float64 `json:"cpuCensoringUtilizationThreshold,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +optional
	MinChildWeight *float64 `json:"minChildWeight,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +optional
	Gamma *float64 `json:"gamma,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +optional
	RegLambda *float64 `json:"regLambda,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +optional
	RegAlpha *float64 `json:"regAlpha,omitempty"`
}

// LightGBM represents a gradient boosted tree configuration driven by LightGBM.
// The runtime predictor trains on aggregate CPU usage history and converts the predicted
// CPU demand back into replicas using the current pod CPU request and target utilization.
type LightGBM struct {
	// useUtilizationFeatures controls optional usage/request lag features.
	// Omitted or true preserves existing behavior. False uses aggregate CPU only.
	// +optional
	UseUtilizationFeatures *bool `json:"useUtilizationFeatures,omitempty"`

	// historySize is how many timestamped replica counts should be stored for this model, older entries are pruned.
	// +kubebuilder:validation:Minimum=1
	HistorySize int `json:"historySize"`

	// lookAhead is how far ahead (in milliseconds) the prediction should forecast.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=9223372036854
	LookAhead int `json:"lookAhead"`

	// lags controls how many previous CPU usage samples are used as features for training.
	// +kubebuilder:validation:Minimum=1
	Lags int `json:"lags"`

	// +kubebuilder:validation:Minimum=1
	// +optional
	WindowSize *int `json:"windowSize,omitempty"`

	// maxDepth follows LightGBM semantics; use -1 for no depth limit or a positive depth.
	// Zero is rejected by controller-side validation.
	// +kubebuilder:validation:Minimum=-1
	// +optional
	MaxDepth *int `json:"maxDepth,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +optional
	NEstimators *int `json:"nEstimators,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:ExclusiveMinimum=true
	// +optional
	LearningRate *float64 `json:"learningRate,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:ExclusiveMinimum=true
	// +kubebuilder:validation:Maximum=1
	// +optional
	Subsample *float64 `json:"subsample,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:ExclusiveMinimum=true
	// +kubebuilder:validation:Maximum=1
	// +optional
	ColsampleBytree *float64 `json:"colsampleBytree,omitempty"`

	// objective defines the LightGBM objective function, defaults to regression.
	// +optional
	Objective *string `json:"objective,omitempty"`

	// +kubebuilder:validation:Minimum=2
	// +optional
	NumLeaves *int `json:"numLeaves,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +optional
	MinChildSamples *int `json:"minChildSamples,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +optional
	RegLambda *float64 `json:"regLambda,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +optional
	RegAlpha *float64 `json:"regAlpha,omitempty"`
}

// Arima represents an ARIMA (AutoRegressive Integrated Moving Average) prediction model configuration
type Arima struct {
	// order is the ARIMA order parameters [p, d, q] where:
	// p = number of lag observations (AR part)
	// d = degree of differencing (I part)
	// q = size of moving average window (MA part)
	// +kubebuilder:validation:items:Minimum=0
	// +kubebuilder:validation:MinItems=3
	// +kubebuilder:validation:MaxItems=3
	Order []int `json:"order"`

	// lookAhead is how far in the future should the ARIMA predict in milliseconds. For example a value of 10000
	// will predict 10 seconds into the future
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=9223372036854
	LookAhead int `json:"lookAhead"`

	// trend specifies the deterministic trend component: 'n' (none), 'c' (constant),
	// 't' (linear), or 'ct' (both). When omitted, the underlying model chooses its default.
	// +kubebuilder:validation:Enum=n;c;t;ct
	// +optional
	Trend *string `json:"trend,omitempty"`

	// autoArima enables automatic ARIMA parameter selection using information criteria
	// +optional
	AutoArima *bool `json:"autoArima,omitempty"`

	// informationCriterion is used when autoArima is true: 'aic' (default) or 'bic'
	// +kubebuilder:validation:Enum=aic;bic
	// +optional
	InformationCriterion *string `json:"informationCriterion,omitempty"`

	// maxOrder defines bounds for automatic ARIMA parameter selection [maxP, maxD, maxQ]
	// +kubebuilder:validation:items:Minimum=0
	// +optional
	// +kubebuilder:validation:MinItems=3
	// +kubebuilder:validation:MaxItems=3
	MaxOrder []int `json:"maxOrder,omitempty"`

	// enforceStationarity forces the AR parameters to be stationary during model fitting
	// +optional
	EnforceStationarity *bool `json:"enforceStationarity,omitempty"`

	// enforceInvertibility forces the MA parameters to be invertible during model fitting
	// +optional
	EnforceInvertibility *bool `json:"enforceInvertibility,omitempty"`

	// concentrateScale concentrates the scale when fitting the model
	// +optional
	ConcentrateScale *bool `json:"concentrateScale,omitempty"`

	// useSarima enables seasonal ARIMA. When true, seasonalOrder and seasonalPeriods must be provided.
	// +optional
	UseSarima *bool `json:"useSarima,omitempty"`

	// seasonalOrder is the seasonal ARIMA order parameters [P, D, Q].
	// +kubebuilder:validation:items:Minimum=0
	// +kubebuilder:validation:MinItems=3
	// +kubebuilder:validation:MaxItems=3
	// +optional
	SeasonalOrder []int `json:"seasonalOrder,omitempty"`

	// seasonalPeriods is the length of the seasonal cycle (m) in sync periods.
	// +kubebuilder:validation:Minimum=2
	// +optional
	SeasonalPeriods *int `json:"seasonalPeriods,omitempty"`

	// historySize is how many timestamped replica counts should be stored for this ARIMA model, with older
	// timestamped replica counts being removed from the data as new ones are added. For example a value of 50
	// means there will only be a maximum of 50 stored timestamped replica counts for this model.
	// Default is 50 if not specified.
	// +kubebuilder:validation:Minimum=3
	// +optional
	HistorySize *int `json:"historySize"`

	// incrementalUpdates enables stateful ARIMA/SARIMA updates between syncs.
	// When enabled, HPA+ can update an existing fitted state with only new observations instead of
	// refitting from scratch on every run.
	// Default is false.
	// +optional
	IncrementalUpdates *bool `json:"incrementalUpdates,omitempty"`

	// refitEvery controls how many incremental updates should occur before forcing a full model refit.
	// When omitted, full refits are only triggered by state resets (e.g. config changes / process restarts).
	// +kubebuilder:validation:Minimum=1
	// +optional
	RefitEvery *int `json:"refitEvery,omitempty"`
}

// PredictivePolicy enables shared time-aware CPU forecast decisions. Future decreases never
// take effect before their timestamps. Tolerance is relative to target capacity.
type PredictivePolicy struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	ScaleUpLeadSeconds int32 `json:"scaleUpLeadSeconds"`
	// tolerance suppresses small upward rounding changes. Omitted/null defaults to 0.1 (10%).
	// Explicit zero disables this tolerance.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=0.2
	// +optional
	Tolerance *float64 `json:"tolerance,omitempty"`
	// scaleUpLeadFromStartup replaces scaleUpLeadSeconds with the longest time a
	// current pod of the target took from creation to readiness, plus five
	// seconds for the decision and scale write, so the lead follows how fast the
	// workload actually starts. scaleUpLeadSeconds applies until a start is
	// observed. Supported with requestDemand.
	// +optional
	ScaleUpLeadFromStartup bool `json:"scaleUpLeadFromStartup,omitempty"`
}

// CPUForecastPoint represents a constant demand estimate on [at, until).
// Strings preserve nanosecond timestamps through history persistence.
type CPUForecastPoint struct {
	At    string `json:"at"`
	Until string `json:"until"`
	CPU   int64  `json:"cpu"`
}

// Model represents a prediction model to use, e.g. a linear regression.
type Model struct {
	// RequestDemand enables live arrival forecasting and learned CPU capacity.
	// +optional
	RequestDemand *RequestDemandConfig `json:"requestDemand,omitempty"`

	// ForecastReliability enables shared error detection and adapter-specific refitting.
	// +optional
	ForecastReliability *ForecastReliabilityPolicy `json:"forecastReliability,omitempty"`

	// predictivePolicy opts into timed decisions; supported by all built-in models and requires predictionMaxAge.
	// Omission preserves scalar forecast behavior.
	// +optional
	PredictivePolicy *PredictivePolicy `json:"predictivePolicy,omitempty"`

	// type is the type of the model, for example 'Linear'. To see a full list of supported model types visit
	// https://github.com/cslab-ntua/HPA-Plus/tree/master/docs/user-guide/models.md.
	// +kubebuilder:validation:Enum=Linear;HoltWinters;ARIMA;XGBoost;LightGBM
	Type string `json:"type"`

	// name is the name of the model, this can be any arbitrary name and is just used to distinguish between models if
	// you have multiple and to keep track of model data if you modify your model parameters.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// sessionID is an internal-only identifier used by the controller/prediction path to scope
	// stateful model runtime sessions (e.g. incremental ARIMA workers).
	// It is intentionally excluded from CRD serialization.
	SessionID string `json:"-"`

	// cpuRequestPerPodMillicores is an internal-only value used by CPU-history based predictors
	// to convert a forecasted aggregate CPU demand back into a replica count.
	CPURequestPerPodMillicores int64 `json:"-"`

	// targetCPUUtilizationPercentage is an internal-only value used by CPU-history based predictors
	// to convert a forecasted aggregate CPU demand back into a replica count.
	TargetCPUUtilizationPercentage int32 `json:"-"`

	// startInterval is the next interval to start applying this model at. This allows you to make sure a model starts
	// recording and being calculated only after a certain interval has passed, e.g. a Holt Winters model that only
	// runs at the top of every hour.
	// This value is a string duration, e.g. 2m30s is 2 minutes and 30 seconds.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^\+?(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+$`
	// +optional
	StartInterval *metav1.Duration `json:"startInterval"`

	// resetDuration is how long can pass without data for the model before the model should reset.
	// This is useful in case a model hasn't been calculated in a long time (e.g. a cluster being powered off) to
	// prevent it from operating on old data and to ensure that the start interval is recalculated.
	// This value is a string duration, e.g. 2m30s is 2 minutes and 30 seconds.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^\+?(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+$`
	// +optional
	ResetDuration *metav1.Duration `json:"resetDuration"`

	// calculationTimeout is how long HPA+ should allow for the model to calculate a value in milliseconds, if it
	// takes longer than this timeout it should skip processing the model.
	// Default is 30000 milliseconds (30 seconds) for every supported family.
	// +kubebuilder:validation:Minimum=1
	// +optional
	// +kubebuilder:validation:Maximum=9223372036854
	CalculationTimeout *int `json:"calculationTimeout"`

	// perSyncPeriod is how frequently this model will run, with the syncPeriod as a base unit. This allows for you to
	// have multiple models which run at different time intervals, or only run the model every x number of sync periods
	// if the model is computation intensive.
	// For sync periods that the model is not run on, it will still add the calculated replica values to the model data
	// history and then prune that history if needs.
	// Default value is 1 (run every sync period)
	// +kubebuilder:validation:Minimum=1
	// +optional
	PerSyncPeriod *int `json:"perSyncPeriod"`

	// predictionMaxAge opts into reusing successful predictions, in milliseconds
	// from their latest consumed sample. Omitted means fresh results only.
	// Reuse is also bounded by the model forecast horizon (one sync period for Holt-Winters).
	// Timed trajectories may use their final forecast interval when maximum age permits.
	// +kubebuilder:validation:Minimum=1
	// +optional
	// +kubebuilder:validation:Maximum=9223372036854
	PredictionMaxAge *int `json:"predictionMaxAge,omitempty"`

	// historyMaxAge limits retained training samples to this age in milliseconds.
	// Omitted keeps count-only retention. Samples expire at the exact age boundary.
	// The window must hold the configured training count at the nominal sampling cadence.
	// +kubebuilder:validation:Minimum=1
	// +optional
	// +kubebuilder:validation:Maximum=9223372036854
	HistoryMaxAge *int `json:"historyMaxAge,omitempty"`

	// linear is the configuration to use for the linear regression model, it will only be used if the type is set to
	// 'Linear'.
	// +optional
	Linear *Linear `json:"linear"`

	// holtWinters is the configuration to use for the holt winters model, it will only be used if the type is set to
	// 'HoltWinters'
	// +optional
	HoltWinters *HoltWinters `json:"holtWinters"`

	// arima is the configuration to use for the ARIMA model, it will only be used if the type is set to
	// 'ARIMA'
	// +optional
	Arima *Arima `json:"arima"`

	// xgboost is the configuration to use for the XGBoost model, it will only be used if the type is set to
	// 'XGBoost'
	// +optional
	XGBoost *XGBoost `json:"xgboost,omitempty"`

	// lightgbm is the configuration to use for the LightGBM model, it will only be used if the type is set to
	// 'LightGBM'
	// +optional
	LightGBM *LightGBM `json:"lightgbm,omitempty"`
}

// TimestampedSample records CPU demand and replica context, or a replica recommendation/event.
// CPU fields are optional only for compatibility with replica-only accounting and legacy training records.
type TimestampedSample struct {
	// time is the time that the replica count was created at.
	Time *metav1.Time `json:"time"`
	// replicas is the replica count at the time.
	Replicas int32 `json:"replicas"`
	// metric is the associated metric value (e.g. CPU utilization percentage) recorded at the time.
	// +optional
	Metric *float64 `json:"metric,omitempty"`
	// totalCPUUsageMillicores is the aggregate CPU usage across the target workload at the recorded time.
	// +optional
	TotalCPUUsageMillicores *int64 `json:"totalCpuUsageMillicores,omitempty"`
	// requestPerPodMillicores is the per-pod CPU request context used when the sample was recorded.
	// +optional
	RequestPerPodMillicores *int64 `json:"requestPerPodMillicores,omitempty"`
	// targetCPUUtilizationPercentage is the CPU target utilization used when the sample was recorded.
	// +optional
	TargetCPUUtilizationPercentage *int32 `json:"targetCpuUtilizationPercentage,omitempty"`
}

// HPAPlusData is the data storage format for HPA+, this is stored in a ConfigMap
type HPAPlusData struct {
	// CPUHistorySource fences measurements from incompatible collection contracts.
	CPUHistorySource string `json:"cpuHistorySource,omitempty"`

	// schemaVersion versions the internal ConfigMap payload; omitted legacy data is version zero.
	SchemaVersion int `json:"schemaVersion,omitempty"`
	// targetIdentity binds training data to the resolved workload reference and UID.
	TargetIdentity string `json:"targetIdentity,omitempty"`
	// modelHistories is a mapping of model names to model histories. This allows looking up a model's model history,
	// while allowing all of the model histories for a single HPA+ object to be stored in a single place.
	ModelHistories map[string]ModelHistory `json:"modelHistories"`
	// pendingScale is a write-ahead record retained until its status accounting is persisted.
	PendingScale *ScaleDecision `json:"pendingScale,omitempty"`
}

// ScaleDecision is internal ConfigMap data, not user-supplied configuration.
type ScaleDecision struct {
	ID                       string                                    `json:"id"`
	OwnerUID                 string                                    `json:"ownerUID"`
	Target                   autoscalingv2.CrossVersionObjectReference `json:"target"`
	TargetUID                string                                    `json:"targetUID"`
	ResourceVersion          string                                    `json:"resourceVersion"`
	PreviousDesired          int32                                     `json:"previousDesired"`
	Desired                  int32                                     `json:"desired"`
	Observed                 int32                                     `json:"observed"`
	Time                     metav1.Time                               `json:"time"`
	ScaleUpRecommendations   []TimestampedSample                       `json:"scaleUpRecommendations"`
	ScaleDownRecommendations []TimestampedSample                       `json:"scaleDownRecommendations"`
}

// ModelHistory is the data stored for a single model's history, with all of the replica data the model is fed to
// calculate replicas.
type ModelHistory struct {
	Reliability *ForecastReliabilityState `json:"reliability,omitempty"`

	// lastObservationID and lastObservationTime identify the last admitted CPU source.
	// Omission is legacy slot sampling; exact RFC3339Nano time survives restarts.
	LastObservationID   string `json:"lastObservationID,omitempty"`
	LastObservationTime string `json:"lastObservationTime,omitempty"`

	// historyEpoch changes on reset so an in-flight fit cannot restore discarded state.
	HistoryEpoch int64 `json:"historyEpoch,omitempty"`
	// lastSampleTime tracks the last sampled slot even when CPU observations were unavailable.
	LastSampleTime *metav1.Time `json:"lastSampleTime,omitempty"`
	// type is the type of the model the history is for, useful to check for model mismatches with the data.
	Type string `json:"type"`
	// syncPeriodsPassed is the number of sync periods that have passed since the last time this model was used, used
	// when determining if a model should run based on the perSyncPeriod
	SyncPeriodsPassed int `json:"syncPeriodsPassed"`
	// replicaHistory is the legacy wire name for timestamped CPU demand samples and replica context.
	ReplicaHistory []TimestampedSample `json:"replicaHistory"`
	// startTime is the time after which the model should start applying and recording data. If it is before this time
	// no data will be recorded and the model will be skipped.
	// +optional
	StartTime *metav1.Time `json:"startTime"`
	// lastModelRunTime is the latest sampled timestamp that this model has already consumed for prediction.
	// +optional
	LastModelRunTime *metav1.Time `json:"lastModelRunTime,omitempty"`
	// cachedPrediction is used only when predictionMaxAge is explicitly configured.
	CachedPrediction *CachedPrediction `json:"cachedPrediction,omitempty"`
	// failureState bounds retries for the same model/configuration/capacity.
	FailureState *ModelFailureState `json:"failureState,omitempty"`
}

// ModelFailureState persists retry scheduling without recording sensitive model errors.
type ModelFailureState struct {
	Fingerprint         string      `json:"fingerprint"`
	ConsecutiveFailures int32       `json:"consecutiveFailures"`
	FailedAt            metav1.Time `json:"failedAt"`
	RetryAfter          metav1.Time `json:"retryAfter"`
}

// CachedPrediction binds a successful forecast to its configuration and capacity.
type CachedPrediction struct {
	Trajectory    []CPUForecastPoint `json:"trajectory,omitempty"`
	Replicas      int32              `json:"replicas"`
	Fingerprint   string             `json:"fingerprint"`
	ConsumedUntil metav1.Time        `json:"consumedUntil"`
	CompletedAt   metav1.Time        `json:"completedAt"`
	ExpiresAt     metav1.Time        `json:"expiresAt"`
	// historyStart retains the oldest input timestamp when age retention is enabled,
	// so rolling count pruning cannot extend the cached forecast's training lifetime.
	HistoryStart *metav1.Time `json:"historyStart,omitempty"`
}

// HPAPlusSpec defines the desired state of HPAPlus
type HPAPlusSpec struct {
	// PredictionFailurePolicy controls unavailable model decisions; omission preserves legacy reactive fallback.
	// +kubebuilder:validation:Enum=HoldCurrent;Reactive
	// +optional
	PredictionFailurePolicy string `json:"predictionFailurePolicy,omitempty"`

	// mode controls rollout: observe never writes Scale, assist never selects below the reactive baseline,
	// control uses decisionType/includeHPA. Omitted/null preserves legacy control behavior.
	// +kubebuilder:validation:Enum=observe;assist;control
	// +optional
	Mode *string `json:"mode,omitempty"`

	// scaleTargetRef points to the target resource to scale, and is used to the pods for which metrics
	// should be collected, as well as to actually change the replica count.
	ScaleTargetRef autoscalingv2.CrossVersionObjectReference `json:"scaleTargetRef"`

	// minReplicas is the lower limit for the number of replicas to which the autoscaler
	// can scale down.  It defaults to 1 pod.  minReplicas is allowed to be 0 if at least one Object or
	// External metric is configured.  Scaling is active as long as at least one metric value is
	// available.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinReplicas *int32 `json:"minReplicas"`

	// maxReplicas is the upper limit for the number of replicas to which the autoscaler can scale up.
	// It cannot be less than minReplicas.
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	// metrics contains the specifications for which to use to calculate the desired replica count (the maximum replica
	// count across all metrics will be used).  The desired replica count is calculated multiplying the ratio between
	// the target value and the current value by the current number of pods.  Ergo, metrics used must decrease as the
	// pod count is increased, and vice-versa.  See the individual metric source types for more information about how
	// each type of metric must respond. Metrics must be configured explicitly; no default metric is injected.
	// +listType=atomic
	// +optional
	Metrics []autoscalingv2.MetricSpec `json:"metrics"`

	// behavior configures the scaling behavior of the target
	// in both Up and Down directions (scaleUp and scaleDown fields respectively).
	// If not set, the default HPAScalingRules for scale up and scale down are used.
	// Both stabilization windows default to zero seconds; explicit windows are preserved.
	// +optional
	Behavior *autoscalingv2.HorizontalPodAutoscalerBehavior `json:"behavior,omitempty"`

	// cpuInitializationPeriod is equivalent to --horizontal-pod-autoscaler-cpu-initialization-period; the period after
	// pod start when CPU samples might be skipped.
	// Default value 300 seconds (5 minutes).
	// +kubebuilder:validation:Minimum=0
	// +optional
	// +kubebuilder:validation:Maximum=9223372036
	CPUInitializationPeriod *int `json:"cpuInitializationPeriod"`

	// initialReadinessDelay is equivalent to --horizontal-pod-autoscaler-initial-readiness-delay; the period after pod
	// start during which readiness changes will be treated as initial readiness.
	// Default value 30 seconds.
	// +kubebuilder:validation:Minimum=0
	// +optional
	// +kubebuilder:validation:Maximum=9223372036
	InitialReadinessDelay *int `json:"initialReadinessDelay"`

	// tolerance is equivalent to --horizontal-pod-autoscaler-tolerance; the minimum change (from 1.0) in the
	// desired-to-actual metrics ratio for the predictive horizontal pod autoscaler to consider scaling.
	// Default value 0.1.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Tolerance *float64 `json:"tolerance"`

	// syncPeriod is equivalent to --horizontal-pod-autoscaler-sync-period; the frequency with which HPA+
	// calculates replica counts and scales in milliseconds.
	// Default value 15000 milliseconds (15 seconds).
	// +kubebuilder:validation:Minimum=1
	// +optional
	// +kubebuilder:validation:Maximum=9223372036854
	SyncPeriod *int `json:"syncPeriod"`

	// metricMaxAge optionally limits source metric age in milliseconds. Unset preserves legacy age behavior.
	// Missing/future timestamps are unavailable when enabled; freshness is checked again before decisions and sampling writes.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=9223372036854
	// +optional
	MetricMaxAge *int `json:"metricMaxAge,omitempty"`

	// models is the list of models to apply to the calculated replica count to calculate predicted replica values.
	// +kubebuilder:validation:Required
	Models []Model `json:"models"`

	// decisionType is the strategy to use when picking which replica count to use if you have multiple models, or even
	// just choosing between the calculculated replicas and the predicted replicas of a single model. For details on
	// which decisionTypes are available visit
	// https://github.com/cslab-ntua/HPA-Plus/tree/master/docs/reference/configuration.md#decisiontype
	// Default strategy is 'maximum'
	// +kubebuilder:validation:Enum=maximum;minimum;mean;median
	// +optional
	DecisionType *string `json:"decisionType"`

	// includeHPA controls whether the baseline HPA calculation joins available model predictions in the decision.
	// It defaults to false. If no model supplies a prediction in the current cycle, the baseline is used regardless
	// of this setting, including during warmup, cadence skips, and forecast failures.
	// +optional
	IncludeHPA *bool `json:"includeHPA,omitempty"`
}

// ModelStatus describes the most recently evaluated eligibility and candidate from a configured model.
type ModelStatus struct {
	// RequestDemand reports the live cost learner and forecast source.
	// +optional
	RequestDemand *RequestDemandStatus `json:"requestDemand,omitempty"`

	// +optional
	Reliability *ForecastReliabilityStatus `json:"reliability,omitempty"`

	// LastAttemptTime and LastSuccessTime describe runtime executions, before persistence/expiry checks.
	// These diagnostic fields do not grant forecast eligibility and may be omitted by the report budget.
	// +optional
	LastAttemptTime *metav1.Time `json:"lastAttemptTime,omitempty"`
	// +optional
	LastSuccessTime *metav1.Time `json:"lastSuccessTime,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	LastRunDurationMilliseconds *int64 `json:"lastRunDurationMilliseconds,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=success;insufficient_history;failed;timeout;canceled
	LastRunOutcome string `json:"lastRunOutcome,omitempty"`

	Name string `json:"name"`
	// +kubebuilder:validation:Enum=Waiting;WarmingUp;Scheduled;Ready;Cached;Backoff;Failed;InvalidHistory;Unavailable;Expired
	State               string       `json:"state"`
	HistorySamples      int64        `json:"historySamples"`
	RequiredSamples     int64        `json:"requiredSamples"`
	ForecastReplicas    *int32       `json:"forecastReplicas,omitempty"`
	ConsumedUntil       *metav1.Time `json:"consumedUntil,omitempty"`
	ExpiresAt           *metav1.Time `json:"expiresAt,omitempty"`
	ConsecutiveFailures int32        `json:"consecutiveFailures,omitempty"`
	RetryAfter          *metav1.Time `json:"retryAfter,omitempty"`
}

// HistoryCapacityStatus is an advisory ConfigMap capacity snapshot, not a reservation.
type HistoryCapacityStatus struct {
	// projectedStateBytes estimates full configured windows plus runtime metadata and current unrelated data.
	// Conservative numeric widths are used; estimates saturate at 9007199254740991 for JSON number compatibility.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=9007199254740991
	ProjectedStateBytes int64 `json:"projectedStateBytes"`
	// projectedJournalBytes estimates a future journal at nominal reconciliation cadence,
	// including at least the currently retained recommendation counts. It is advisory, not a reservation.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=9007199254740991
	ProjectedJournalBytes int64 `json:"projectedJournalBytes,omitempty"`
	// currentStateBytes is normalized state plus unrelated ConfigMap data, excluding the journal.
	// +kubebuilder:validation:Minimum=0
	CurrentStateBytes int64 `json:"currentStateBytes"`
	// currentJournalBytes is the normalized pending journal size including field framing.
	// +kubebuilder:validation:Minimum=0
	CurrentJournalBytes int64 `json:"currentJournalBytes"`
	// +kubebuilder:validation:Minimum=0
	StateBudgetBytes int64 `json:"stateBudgetBytes"`
	// +kubebuilder:validation:Minimum=0
	JournalBudgetBytes int64 `json:"journalBudgetBytes"`
}

// HPAPlusStatus defines the observed state of HPAPlus.
type HPAPlusStatus struct {
	// lastDecision is bounded, generation-scoped evidence from the latest evaluated plan.
	// It is not a durable command and does not maintain simulated stabilization histories.
	// +optional
	LastDecision *DecisionStatus `json:"lastDecision,omitempty"`

	// historyCapacity reports capacity at the history read in this reconciliation. It does not limit sampling or scaling.
	// +optional
	HistoryCapacity *HistoryCapacityStatus `json:"historyCapacity,omitempty"`
	// observedGeneration is the specification generation assessed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// conditions describe configuration, metrics, predictions and reconciliation health.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// modelStatuses reports up to 64 models, with a combined encoded budget of 16 KiB.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	// +optional
	ModelStatuses []ModelStatus `json:"modelStatuses,omitempty"`
	// modelStatusesOmitted counts models omitted from the bounded detailed report.
	// +optional
	ModelStatusesOmitted int32 `json:"modelStatusesOmitted,omitempty"`
	// targetIdentity identifies the workload whose scaling history is recorded here.
	// +optional
	TargetIdentity string `json:"targetIdentity,omitempty"`
	// lastScaleDecisionID prevents duplicate accounting when recovering a scale/status partial write.
	// +optional
	LastScaleDecisionID string `json:"lastScaleDecisionID,omitempty"`
	// lastScaleTime is the last time the HPAPlus scaled the number of pods,
	// used by the autoscaler to control how often the number of pods is changed.
	// +optional
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`

	// scaleUpReplicaHistory is a list of timestamped replicas within the scale up stabilization window.
	// Used for calculating upscale stabilization.
	// +optional
	ScaleUpReplicaHistory []TimestampedSample `json:"scaleUpReplicaHistory"`

	// scaleDownReplicaHistory is a list of timestamped replicas within the scale down stabilization window.
	// Used for calculating downscale stabilization.
	// +optional
	ScaleDownReplicaHistory []TimestampedSample `json:"scaleDownReplicaHistory"`

	// scaleUpEventHistory is a list of timestamped changes in replicas for every time a scale up event occurs for
	// this resource. A value of 5 means that at that scale event the resource was scaled up by 5 replicas.
	// Used for applying scale up policies.
	// +optional
	ScaleUpEventHistory []TimestampedSample `json:"scaleUpEventHistory"`

	// scaleDownEventHistory is a list of timestamped changes in replicas for every time a scale down event occurs for
	// this resource. A value of 5 means that at that scale event the resource was scaled down by 5 replicas.
	// Used for applying scale down policies.
	// +optional
	ScaleDownEventHistory []TimestampedSample `json:"scaleDownEventHistory"`

	// reference is the resource being referenced and targeted for scaling.
	Reference string `json:"reference"`

	// currentReplicas is current number of replicas of pods managed by this autoscaler,
	// as last seen by the autoscaler.
	// +optional
	CurrentReplicas int32 `json:"currentReplicas,omitempty"`

	// desiredReplicas is the desired number of replicas of pods managed by this autoscaler,
	// as last calculated by the autoscaler.
	DesiredReplicas int32 `json:"desiredReplicas"`

	// currentMetrics reports gathered values before missing-pod substitutions, with up to 64 entries and 16 KiB.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	// +optional
	CurrentMetrics []autoscalingv2.MetricStatus `json:"currentMetrics"`
	// currentMetricsOmitted counts gathered metrics excluded from the detailed report.
	// +optional
	CurrentMetricsOmitted int32 `json:"currentMetricsOmitted,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=hpapluses,singular=hpaplus,shortName=hpaplus
// +kubebuilder:printcolumn:name="Reference",type="string",JSONPath=`.status.reference`,description="The identifier for the resource being scaled in the format <api-version>/<api-kind/<name>"
// +kubebuilder:printcolumn:name="Min Pods",type="integer",JSONPath=`.spec.minReplicas`,description="The minimum number of replicas of pods that the resource being managed by the autoscaler can have"
// +kubebuilder:printcolumn:name="Max Pods",type="integer",JSONPath=`.spec.maxReplicas`,description="The maximum number of replicas of pods that the resource being managed by the autoscaler can have"
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=`.status.desiredReplicas`,description="The desired number of replicas of pods managed by this autoscaler as last calculated by the autoscaler"
// +kubebuilder:printcolumn:name="Last Scale Time",type="date",JSONPath=`.status.lastScaleTime`,description="The last time the HPAPlus scaled the number of pods"
// HPAPlus is the Schema for the hpapluses API
type HPAPlus struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HPAPlusSpec   `json:"spec,omitempty"`
	Status HPAPlusStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// HPAPlusList contains a list of HPAPlus
type HPAPlusList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HPAPlus `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HPAPlus{}, &HPAPlusList{})
}
