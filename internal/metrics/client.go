/*
Copyright 2022 The K8sHorizMetrics Authors.

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

package metrics

import (
	"context"
	"fmt"
	"github.com/jthomperoo/k8shorizmetrics/v2/metrics/podmetrics"
	"github.com/jthomperoo/k8shorizmetrics/v2/metricsclient"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	custommetricsv1 "k8s.io/metrics/pkg/apis/custom_metrics/v1beta2"
	"math"
	"time"
)

// Adapted from k8shorizmetrics v2.0.1. Check quantities before MilliValue and
// external/container sums before the legacy gatherer can lose overflow evidence.
type checkedMetricClient struct {
	*metricsclient.RESTClient
	ctx    context.Context
	pods   *rawPodLister
	cgroup *cgroupSource
}

// GetResourceMetric gets the given resource metric (and an associated oldest timestamp)
// for all pods matching the specified selector in the given namespace
func (c *checkedMetricClient) GetResourceMetric(resource v1.ResourceName, namespace string, selector labels.Selector) (podmetrics.MetricsInfo, time.Time, error) {
	if c.cgroup != nil && (resource == v1.ResourceCPU || resource == v1.ResourceMemory) {
		return c.cgroup.podMetrics(c.ctx, c.pods, resource, namespace, selector)
	}
	metrics, err := c.Client.PodMetricses(namespace).List(c.ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("unable to fetch metrics from resource metrics API: %v", err)
	}

	if len(metrics.Items) == 0 {
		return nil, time.Time{}, fmt.Errorf("no metrics returned from resource metrics API")
	}

	// Require a complete set of serving container samples, including native
	// sidecars. A pod aggregate with a missing container is not complete CPU demand.
	var expected map[string]map[string]bool
	if c.pods != nil {
		objects, err := c.pods.readPods(namespace, selector)
		if err != nil {
			return nil, time.Time{}, err
		}
		expected = make(map[string]map[string]bool, len(objects))
		for _, obj := range objects {
			var view struct {
				Spec requestSpec `json:"spec"`
			}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &view); err != nil {
				return nil, time.Time{}, err
			}
			containers, err := view.Spec.runningContainers()
			if err != nil {
				return nil, time.Time{}, err
			}
			names := map[string]bool{}
			for _, container := range containers {
				names[container.Name] = true
			}
			expected[obj.GetName()] = names
		}
	}
	res := make(podmetrics.MetricsInfo, len(metrics.Items))
	for _, m := range metrics.Items {
		if expected != nil {
			names, found := expected[m.Name]
			if !found || len(names) != len(m.Containers) {
				continue
			}
			seen := map[string]bool{}
			complete := true
			for _, container := range m.Containers {
				if !names[container.Name] || seen[container.Name] {
					complete = false
					break
				}
				seen[container.Name] = true
			}
			if !complete {
				continue
			}
		}
		podSum := int64(0)
		missing := len(m.Containers) == 0
		for _, c := range m.Containers {
			resValue, found := c.Usage[resource]
			if !found {
				missing = true
				break
			}
			value, err := milliValue(&resValue, false)
			if err != nil {
				return nil, time.Time{}, err
			}
			podSum, err = addMetric(podSum, value)
			if err != nil {
				return nil, time.Time{}, err
			}
		}
		if !missing {
			if err := checkTimestamp(c.ctx, m.Timestamp.Time); err != nil {
				return nil, time.Time{}, err
			}
			if m.Window.Duration < 0 {
				return nil, time.Time{}, fmt.Errorf("invalid resource metric window")
			}
			res[m.Name] = podmetrics.Metric{
				Timestamp: m.Timestamp.Time,
				Window:    m.Window.Duration,
				Value:     podSum,
			}
		}
	}

	timestamp := oldestPodTimestamp(res)

	return res, timestamp, nil
}

// GetRawMetric gets the given metric (and an associated oldest timestamp)
// for all pods matching the specified selector in the given namespace
func (c *checkedMetricClient) GetRawMetric(metricName string, namespace string, selector labels.Selector, metricSelector labels.Selector) (podmetrics.MetricsInfo, time.Time, error) {
	metrics, err := c.CustomMetricsClient.NamespacedMetrics(namespace).GetForObjects(schema.GroupKind{Kind: "Pod"}, selector, metricName, metricSelector)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("unable to fetch metrics from custom metrics API: %v", err)
	}

	if len(metrics.Items) == 0 {
		return nil, time.Time{}, fmt.Errorf("no metrics returned from custom metrics API")
	}

	res := make(podmetrics.MetricsInfo, len(metrics.Items))
	for _, m := range metrics.Items {
		if err := checkTimestamp(c.ctx, m.Timestamp.Time); err != nil {
			return nil, time.Time{}, err
		}
		window := time.Minute
		if m.WindowSeconds != nil {
			if *m.WindowSeconds < 0 || *m.WindowSeconds > math.MaxInt64/int64(time.Second) {
				return nil, time.Time{}, fmt.Errorf("invalid metric window")
			}
			window = time.Duration(*m.WindowSeconds) * time.Second
		}
		value, err := milliValue(&m.Value, false)
		if err != nil {
			return nil, time.Time{}, err
		}
		res[m.DescribedObject.Name] = podmetrics.Metric{
			Timestamp: m.Timestamp.Time,
			Window:    window,
			Value:     value,
		}

	}

	timestamp := oldestPodTimestamp(res)

	return res, timestamp, nil
}

// GetObjectMetric gets the given metric (and an associated timestamp) for the given
// object in the given namespace
func (c *checkedMetricClient) GetObjectMetric(metricName string, namespace string, objectRef *autoscalingv2.CrossVersionObjectReference, metricSelector labels.Selector) (int64, time.Time, error) {
	gvk := schema.FromAPIVersionAndKind(objectRef.APIVersion, objectRef.Kind)
	var metricValue *custommetricsv1.MetricValue
	var err error
	if gvk.Kind == "Namespace" && gvk.Group == "" {
		// handle namespace separately
		// NB: we ignore namespace name here, since CrossVersionObjectReference isn't
		// supposed to allow you to escape your namespace
		metricValue, err = c.CustomMetricsClient.RootScopedMetrics().GetForObject(gvk.GroupKind(), namespace, metricName, metricSelector)
	} else {
		metricValue, err = c.CustomMetricsClient.NamespacedMetrics(namespace).GetForObject(gvk.GroupKind(), objectRef.Name, metricName, metricSelector)
	}

	if err != nil {
		return 0, time.Time{}, fmt.Errorf("unable to fetch metrics from custom metrics API: %v", err)
	}

	if metricValue == nil {
		return 0, time.Time{}, fmt.Errorf("missing object metric value")
	}
	if err := checkTimestamp(c.ctx, metricValue.Timestamp.Time); err != nil {
		return 0, time.Time{}, err
	}
	value, err := milliValue(&metricValue.Value, false)
	return value, metricValue.Timestamp.Time, err
}

// GetExternalMetric gets all the values of a given external metric
// that match the specified selector.
func (c *checkedMetricClient) GetExternalMetric(metricName, namespace string, selector labels.Selector) ([]int64, time.Time, error) {
	metrics, err := c.ExternalMetricsClient.NamespacedMetrics(namespace).List(metricName, selector)
	if err != nil {
		return []int64{}, time.Time{}, fmt.Errorf("unable to fetch metrics from external metrics API: %v", err)
	}

	if len(metrics.Items) == 0 {
		return nil, time.Time{}, fmt.Errorf("no metrics returned from external metrics API")
	}

	res := make([]int64, 0, len(metrics.Items))
	total := int64(0)
	timestamp := metrics.Items[0].Timestamp.Time
	for _, m := range metrics.Items {
		if err := checkTimestamp(c.ctx, m.Timestamp.Time); err != nil {
			return nil, time.Time{}, err
		}
		if m.Timestamp.Time.Before(timestamp) {
			timestamp = m.Timestamp.Time
		}
		value, err := milliValue(&m.Value, false)
		if err != nil {
			return nil, time.Time{}, err
		}
		total, err = addMetric(total, value)
		if err != nil {
			return nil, time.Time{}, err
		}
		res = append(res, value)
	}
	return res, timestamp, nil
}
