package validation

import (
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"golang.org/x/net/http/httpguts"
	v2 "k8s.io/api/autoscaling/v2"
	pathvalidation "k8s.io/apimachinery/pkg/api/validation/path"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
)

func validateHook(hook *api.HookDefinition) error {
	if hook.Type != api.HookTypeHTTP || hook.HTTP == nil {
		return fmt.Errorf("runtimeTuningFetchHook requires type http and an HTTP configuration")
	}
	if !validDuration(hook.Timeout, time.Millisecond, 1) {
		return fmt.Errorf("hook timeout must be a positive representable millisecond duration")
	}
	h := hook.HTTP
	u, err := url.Parse(h.URL)
	if err != nil || u.Hostname() == "" || !oneOf(u.Scheme, "http", "https") || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("hook URL must be an absolute HTTP(S) URL without credentials or a fragment")
	}
	if !oneOf(h.Method, "GET", "HEAD", "POST", "PUT", "DELETE", "CONNECT", "OPTIONS", "TRACE", "PATCH") {
		return fmt.Errorf("unsupported hook HTTP method")
	}
	if !oneOf(h.ParameterMode, "query", "body") {
		return fmt.Errorf("hook parameterMode must be query or body")
	}
	if len(h.SuccessCodes) == 0 {
		return fmt.Errorf("hook successCodes must not be empty")
	}
	for _, code := range h.SuccessCodes {
		if code < 100 || code > 599 {
			return fmt.Errorf("hook successCodes must be between 100 and 599")
		}
	}
	for name, value := range h.Headers {
		if !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
			return fmt.Errorf("hook headers contain an invalid name or value")
		}
	}
	return nil
}

func validateInitialization(hw *api.HoltWinters) error {
	method := "estimated"
	if hw.InitializationMethod != nil {
		method = *hw.InitializationMethod
	}
	if !oneOf(method, "estimated", "heuristic", "known", "legacy-heuristic") {
		return fmt.Errorf("invalid Holt-Winters initializationMethod")
	}
	for _, value := range []*float64{hw.InitialLevel, hw.InitialTrend, hw.InitialSeasonal} {
		if method == "known" && value == nil {
			return fmt.Errorf("known initialization requires initialLevel, initialTrend, and initialSeasonal")
		}
		if method != "known" && value != nil {
			return fmt.Errorf("initial values require initializationMethod known")
		}
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return fmt.Errorf("Holt-Winters initial values must be finite")
		}
	}
	return nil
}

func validateReference(ref v2.CrossVersionObjectReference) error {
	if strings.TrimSpace(ref.Name) == "" || strings.TrimSpace(ref.Kind) == "" ||
		len(pathvalidation.IsValidPathSegmentName(ref.Name)) != 0 || len(pathvalidation.IsValidPathSegmentName(ref.Kind)) != 0 {
		return fmt.Errorf("target reference requires valid non-empty name and kind")
	}
	// An omitted version is resolved by the REST mapper to the preferred version.
	if ref.APIVersion != "" {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil || gv.Version == "" || len(utilvalidation.IsDNS1035Label(gv.Version)) != 0 ||
			(gv.Group != "" && len(utilvalidation.IsDNS1123Subdomain(gv.Group)) != 0) {
			return fmt.Errorf("invalid target apiVersion")
		}
	}
	return nil
}

func validateKubernetesSpec(spec api.HPAPlusSpec) error {
	if err := validateReference(spec.ScaleTargetRef); err != nil {
		return fmt.Errorf("scaleTargetRef: %w", err)
	}
	if len(spec.Metrics) == 0 {
		return fmt.Errorf("at least one metric is required; no default metric is injected")
	}
	cpuCount := 0
	for i, metric := range spec.Metrics {
		if err := validateMetric(metric); err != nil {
			return fmt.Errorf("metrics[%d]: %w", i, err)
		}
		if metric.Resource != nil && metric.Resource.Name == "cpu" {
			cpuCount++
		}
	}
	if cpuCount > 1 {
		return fmt.Errorf("only one CPU resource metric is allowed to avoid ambiguous forecast capacity")
	}
	if spec.Behavior != nil {
		for _, rules := range []*v2.HPAScalingRules{spec.Behavior.ScaleUp, spec.Behavior.ScaleDown} {
			if err := validateRules(rules); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateMetric(m v2.MetricSpec) error {
	count := 0
	for _, present := range []bool{m.Resource != nil, m.ContainerResource != nil, m.Pods != nil, m.Object != nil, m.External != nil} {
		if present {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("exactly one metric source is required")
	}
	var target v2.MetricTarget
	var identifier *v2.MetricIdentifier
	var allowed []v2.MetricTargetType
	switch {
	case m.Type == v2.ResourceMetricSourceType && m.Resource != nil:
		if len(utilvalidation.IsQualifiedName(string(m.Resource.Name))) != 0 {
			return fmt.Errorf("invalid resource metric name")
		}
		target, allowed = m.Resource.Target, []v2.MetricTargetType{v2.UtilizationMetricType, v2.AverageValueMetricType}
	case m.Type == v2.PodsMetricSourceType && m.Pods != nil:
		target, identifier, allowed = m.Pods.Target, &m.Pods.Metric, []v2.MetricTargetType{v2.AverageValueMetricType}
	case m.Type == v2.ObjectMetricSourceType && m.Object != nil:
		if err := validateReference(m.Object.DescribedObject); err != nil {
			return err
		}
		target, identifier, allowed = m.Object.Target, &m.Object.Metric, []v2.MetricTargetType{v2.ValueMetricType, v2.AverageValueMetricType}
	case m.Type == v2.ExternalMetricSourceType && m.External != nil:
		target, identifier, allowed = m.External.Target, &m.External.Metric, []v2.MetricTargetType{v2.ValueMetricType, v2.AverageValueMetricType}
	default:
		return fmt.Errorf("unsupported or mismatched metric source type %q", m.Type)
	}
	if identifier != nil {
		if strings.TrimSpace(identifier.Name) == "" || len(pathvalidation.IsValidPathSegmentName(identifier.Name)) != 0 {
			return fmt.Errorf("invalid metric name")
		}
		if _, err := metav1.LabelSelectorAsSelector(identifier.Selector); err != nil {
			return fmt.Errorf("invalid metric selector: %w", err)
		}
	}
	count = 0
	for _, present := range []bool{target.Value != nil, target.AverageValue != nil, target.AverageUtilization != nil} {
		if present {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("exactly one metric target value is required")
	}
	for _, kind := range allowed {
		if target.Type != kind {
			continue
		}
		switch kind {
		case v2.ValueMetricType:
			if target.Value != nil && target.Value.Sign() > 0 {
				return nil
			}
		case v2.AverageValueMetricType:
			if target.AverageValue != nil && target.AverageValue.Sign() > 0 {
				return nil
			}
		case v2.UtilizationMetricType:
			if target.AverageUtilization != nil && *target.AverageUtilization > 0 {
				return nil
			}
		}
	}
	return fmt.Errorf("metric target type must match its source and have a positive value")
}

func validateRules(rules *v2.HPAScalingRules) error {
	if rules == nil {
		return nil
	}
	if rules.StabilizationWindowSeconds != nil && (*rules.StabilizationWindowSeconds < 0 || *rules.StabilizationWindowSeconds > 3600) {
		return fmt.Errorf("stabilizationWindowSeconds must be in [0, 3600]")
	}
	if rules.SelectPolicy != nil && !oneOf(string(*rules.SelectPolicy), "Max", "Min", "Disabled") {
		return fmt.Errorf("selectPolicy must be Max, Min, or Disabled")
	}
	if rules.Policies != nil && len(rules.Policies) == 0 {
		return fmt.Errorf("policies must be omitted for defaults or contain at least one policy")
	}
	for _, policy := range rules.Policies {
		if !oneOf(string(policy.Type), "Pods", "Percent") || policy.Value <= 0 || policy.PeriodSeconds < 1 || policy.PeriodSeconds > 1800 {
			return fmt.Errorf("scaling policy requires Pods/Percent, positive value, and periodSeconds in [1, 1800]")
		}
	}
	return nil
}
