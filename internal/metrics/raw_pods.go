package metrics

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	corelisters "k8s.io/client-go/listers/core/v1"
)

var podResource = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

type rawPodLister struct {
	client    dynamic.Interface
	ctx       context.Context
	resources map[corev1.ResourceName]bool
	// One gather uses one selector and runs metric specs sequentially. Retain the
	// raw list so metric coverage and request denominators see the same pods.
	listed    bool
	objects   []unstructured.Unstructured
	listError error
	// stopping also lists pods that are being deleted. CPU history needs them:
	// a pod deleted as a window closes served that whole window.
	stopping bool
}

func (l *rawPodLister) Pods(namespace string) corelisters.PodNamespaceLister {
	return &rawPodNamespaceLister{parent: l, namespace: namespace}
}
func (l *rawPodLister) List(selector labels.Selector) ([]*corev1.Pod, error) {
	return l.Pods("").List(selector)
}

type rawPodNamespaceLister struct {
	parent    *rawPodLister
	namespace string
}

func (l *rawPodNamespaceLister) List(selector labels.Selector) ([]*corev1.Pod, error) {
	objects, err := l.parent.readPods(l.namespace, selector)
	if err != nil {
		return nil, err
	}
	pods := make([]*corev1.Pod, 0, len(objects))
	for i := range objects {
		pod, err := projectPod(&objects[i], l.parent.resources)
		if err != nil {
			return nil, err
		}
		pods = append(pods, pod)
	}
	return pods, nil
}

// This lister is private to one Gather, whose namespace/selector never change.
func (l *rawPodLister) readPods(namespace string, selector labels.Selector) ([]unstructured.Unstructured, error) {
	if !l.listed {
		l.listed = true
		list, err := l.client.Resource(podResource).Namespace(namespace).List(l.ctx, metav1.ListOptions{LabelSelector: selector.String()})
		l.listError = err
		if err == nil {
			for _, obj := range list.Items {
				phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
				if obj.GetDeletionTimestamp() != nil {
					// A deleted pod may already report a terminal phase.
					if !l.stopping {
						continue
					}
				} else if phase == "Failed" || phase == "Succeeded" {
					continue
				}
				l.objects = append(l.objects, obj)
			}
		}
	}
	return l.objects, l.listError
}

func (l *rawPodNamespaceLister) Get(name string) (*corev1.Pod, error) {
	obj, err := l.parent.client.Resource(podResource).Namespace(l.namespace).Get(l.parent.ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return projectPod(obj, l.parent.resources)
}

func projectPod(obj *unstructured.Unstructured, names map[corev1.ResourceName]bool) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, pod); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return pod, nil
	}
	if err := validatePodResize(obj.Object, names); err != nil {
		return nil, fmt.Errorf("pod %s: %w", pod.Name, err)
	}
	spec, _, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil {
		return nil, err
	}
	requests := corev1.ResourceList{}
	for name := range names {
		value, err := PodResourceRequest(spec, name)
		if err != nil {
			return nil, fmt.Errorf("pod %s: %w", pod.Name, err)
		}
		requests[name] = *resource.NewMilliQuantity(value, resource.DecimalSI)
	}
	// The legacy evaluator only sums regular containers. Give it an isolated
	// request carrier; metadata/readiness stay intact. Never write this projection
	// to Kubernetes or use it for container-specific metrics.
	pod.Spec.Containers = []corev1.Container{{Name: "hpaplus-capacity", Resources: corev1.ResourceRequirements{Requests: requests}}}
	return pod, nil
}
