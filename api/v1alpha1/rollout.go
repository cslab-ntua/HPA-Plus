package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	ModeObserve = "observe"
	ModeAssist  = "assist"
	ModeControl = "control"
)

// EffectiveMode preserves pre-mode v1alpha1 behavior for omitted/null mode.
// New installations should explicitly begin with observe.
func (s HPAPlusSpec) EffectiveMode() string {
	if s.Mode == nil {
		return ModeControl
	}
	return *s.Mode
}

// DecisionStatus is the latest bounded decision evidence, not a durable command
// or a counterfactual time series. All plans use the same observed real history.
type DecisionStatus struct {
	// evidence captures bounded inputs and constraints at evaluation time. Older
	// operators omit it; consumers must not reconstruct it from later status.
	// +optional
	Evidence *DecisionEvidence `json:"evidence,omitempty"`
	// +kubebuilder:validation:MaxLength=64
	ID string `json:"id"`
	// +kubebuilder:validation:Enum=observe;assist;control
	Mode string `json:"mode"`
	// +kubebuilder:validation:Enum=baseline;predictive;mixed;hold
	CandidateMode string `json:"candidateMode"`
	// +kubebuilder:validation:Enum=not_attempted;observed;unchanged;accepted;rejected;unknown
	ScaleWrite         string      `json:"scaleWrite"`
	EvaluatedAt        metav1.Time `json:"evaluatedAt"`
	ObservedGeneration int64       `json:"observedGeneration"`
	// +kubebuilder:validation:MaxLength=128
	TargetUID         string `json:"targetUID"`
	RequestedReplicas int32  `json:"requestedReplicas"`
	ObservedReplicas  int32  `json:"observedReplicas"`
	// baselineReplicas is the raw reactive recommendation before behavior.
	BaselineReplicas int32 `json:"baselineReplicas"`
	// baselineDesiredReplicas is the reactive-only plan after safety constraints.
	BaselineDesiredReplicas int32 `json:"baselineDesiredReplicas"`
	// controlDesiredReplicas is the configured control plan, including includeHPA.
	ControlDesiredReplicas int32 `json:"controlDesiredReplicas"`
	// desiredReplicas is requested count in observe, otherwise this cycle's plan.
	// An accepted write can still fail later accounting; consult scaleWrite/journal.
	DesiredReplicas int32 `json:"desiredReplicas"`
}

type DecisionCandidate struct {
	// +kubebuilder:validation:MaxLength=256
	Model    string `json:"model"`
	Replicas int32  `json:"replicas"`
}

type DecisionEvidence struct {
	// models captures eligibility and source/expiry times at evaluation time,
	// independently of the latest status.modelStatuses.
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	Models []ModelStatus `json:"models,omitempty"`
	// +kubebuilder:validation:Minimum=0
	ModelsOmitted    int32 `json:"modelsOmitted,omitempty"`
	SelectedReplicas int32 `json:"selectedReplicas"`
	GuardedReplicas  int32 `json:"guardedReplicas"`
	BaselineIncluded bool  `json:"baselineIncluded"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	Constraints []string `json:"constraints"`
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	Candidates []DecisionCandidate `json:"candidates"`
	// +kubebuilder:validation:Minimum=0
	CandidatesOmitted int32 `json:"candidatesOmitted"`
}
