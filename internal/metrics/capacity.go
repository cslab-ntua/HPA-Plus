package metrics

import (
	"fmt"
	"math"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
)

// requestSpec preserves resource fields introduced after the pinned Kubernetes
// Go types. Decode from the original JSON object, never through corev1.PodSpec.
type requestSpec struct {
	Containers     []requestContainer           `json:"containers"`
	InitContainers []requestContainer           `json:"initContainers"`
	Resources      *corev1.ResourceRequirements `json:"resources,omitempty"`
}
type requestContainer struct {
	Name          string                      `json:"name"`
	Resources     corev1.ResourceRequirements `json:"resources"`
	RestartPolicy *string                     `json:"restartPolicy,omitempty"`
}

// PodResourceRequest returns steady-state capacity in milli-units. A declared
// Pod-level request overrides container sums for that resource. Otherwise all
// regular and restartable init containers must have positive requests. Classic
// init peaks and runtime overhead are not application capacity.
func PodResourceRequest(spec map[string]interface{}, name corev1.ResourceName) (int64, error) {
	var view requestSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec, &view); err != nil {
		return 0, fmt.Errorf("invalid pod resource specification: %w", err)
	}
	return view.request(name)
}

func (s requestSpec) runningContainers() ([]requestContainer, error) {
	containers := append([]requestContainer(nil), s.Containers...)
	for _, c := range s.InitContainers {
		if c.RestartPolicy == nil {
			continue
		}
		if *c.RestartPolicy != "Always" {
			return nil, fmt.Errorf("unsupported init container restart policy")
		}
		containers = append(containers, c)
	}
	if len(s.Containers) == 0 {
		return nil, fmt.Errorf("pod has no application containers")
	}
	return containers, nil
}

func (s requestSpec) request(name corev1.ResourceName) (int64, error) {
	containers, err := s.runningContainers()
	if err != nil {
		return 0, err
	}
	if s.Resources != nil {
		if q, ok := s.Resources.Requests[name]; ok {
			return positiveRequest(q)
		}
	}
	total := int64(0)
	for _, c := range containers {
		q, ok := c.Resources.Requests[name]
		if !ok {
			return 0, fmt.Errorf("missing %s request for container %s", name, c.Name)
		}
		value, err := positiveRequest(q)
		if err != nil {
			return 0, err
		}
		if value > math.MaxInt64-total {
			return 0, fmt.Errorf("pod resource request sum overflows")
		}
		total += value
	}
	return total, nil
}

func positiveRequest(q resource.Quantity) (int64, error) {
	if q.Sign() <= 0 || q.Cmp(*resource.NewMilliQuantity(math.MaxInt64, resource.DecimalSI)) > 0 {
		return 0, fmt.Errorf("resource request must be positive and representable in milli-units")
	}
	return q.MilliValue(), nil
}

// Refuse utilization of desired requests during a known in-place resize. Status
// fields are optional on older servers; when present, actual/allocated requests
// must agree with spec. This is a fail-closed capacity policy, not a VPA controller.
func validatePodResize(object map[string]interface{}, names map[corev1.ResourceName]bool) error {
	var view struct {
		Spec   requestSpec `json:"spec"`
		Status struct {
			Resize                string                       `json:"resize"`
			Conditions            []corev1.PodCondition        `json:"conditions"`
			Resources             *corev1.ResourceRequirements `json:"resources"`
			ContainerStatuses     []requestStatus              `json:"containerStatuses"`
			InitContainerStatuses []requestStatus              `json:"initContainerStatuses"`
		} `json:"status"`
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object, &view); err != nil {
		return err
	}
	if view.Status.Resize != "" {
		return fmt.Errorf("pod resource resize has not settled")
	}
	for _, c := range view.Status.Conditions {
		if (c.Type == "PodResizePending" || c.Type == "PodResizeInProgress") && c.Status == corev1.ConditionTrue {
			return fmt.Errorf("pod resource resize has not settled")
		}
	}
	containers, err := view.Spec.runningContainers()
	if err != nil {
		return err
	}
	// A Pod-level budget is the denominator even if individual containers
	// inherit requests or are resized inside that budget. Compare their status
	// only for resources whose denominator actually comes from container sums.
	containerNames := map[corev1.ResourceName]bool{}
	for name := range names {
		if view.Spec.Resources != nil {
			if _, override := view.Spec.Resources.Requests[name]; override {
				continue
			}
		}
		containerNames[name] = true
	}
	desired := map[string]corev1.ResourceList{}
	for _, c := range containers {
		desired[c.Name] = c.Resources.Requests
	}
	for _, status := range append(view.Status.ContainerStatuses, view.Status.InitContainerStatuses...) {
		requests, running := desired[status.Name]
		if !running {
			continue
		}
		if err := compareRequests(requests, status.AllocatedResources, containerNames); err != nil {
			return err
		}
		if status.Resources != nil {
			if err := compareRequests(requests, status.Resources.Requests, containerNames); err != nil {
				return err
			}
		}
	}
	if view.Status.Resources != nil {
		var requests corev1.ResourceList
		if view.Spec.Resources != nil {
			requests = view.Spec.Resources.Requests
		}
		return compareRequests(requests, view.Status.Resources.Requests, names)
	}
	return nil
}

type requestStatus struct {
	Name               string                       `json:"name"`
	Resources          *corev1.ResourceRequirements `json:"resources"`
	AllocatedResources corev1.ResourceList          `json:"allocatedResources"`
}

func compareRequests(desired, actual corev1.ResourceList, names map[corev1.ResourceName]bool) error {
	for name := range names {
		if current, found := actual[name]; found {
			requested, exists := desired[name]
			if !exists || requested.Cmp(current) != 0 {
				return fmt.Errorf("pod %s request differs from allocated or running capacity", name)
			}
		}
	}
	return nil
}
