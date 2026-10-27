package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/cgroup"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type demandOutcomeAccountingGatherer struct {
	demandIntegrationGatherer
	now *time.Time
}

func (g *demandOutcomeAccountingGatherer) GatherCgroupResources(_ context.Context, _ string, _ labels.Selector, from, to time.Time) (*cgroup.WorkloadObservation, error) {
	window, _ := g.GatherCgroupPodResources(context.Background(), "a", "app-pod", "pod-a", from, to)
	window.InventoryComplete = true
	window.Inventory = []cgroup.Pod{{Namespace: "a", Name: "app-pod", UID: "pod-a", Containers: []cgroup.Container{{ID: "container-a", StartedAt: from.Add(-time.Hour)}}}}
	return window, nil
}

func (g *demandOutcomeAccountingGatherer) GatherCgroupPodResources(_ context.Context, _, _, _ string, from, to time.Time) (*cgroup.WorkloadObservation, error) {
	return &cgroup.WorkloadObservation{From: from, To: to, Complete: true, Coverage: 1, CPUSeconds: .5 * to.Sub(from).Seconds(), RunningPodSeconds: to.Sub(from).Seconds(), SourceAt: to, ReceivedAt: *g.now}, nil
}

func TestDemandAccountingErrorGuardKeepsProcessedWorkDenominator(t *testing.T) {
	for _, tc := range []struct {
		name             string
		work, errors     float64
		includeOutcomes  bool
		wantContaminated bool
	}{
		// 1/101 finished outcomes is below 1%, but 1/90 units of processed
		// work exceeds it. Adding the optional denominator cannot weaken the
		// existing CPU-cost accounting guard.
		{"different denominators", 90, 1, true, true},
		{"exact original boundary", 100, 1, true, false},
		{"optional denominator absent", 90, 1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, owner, scale, now := demandIntegrationFixture(t)
			ctx := context.Background()
			model := &owner.Spec.Models[0]
			model.RequestDemand.Traffic.ErrorsMetric = "errors_total"
			if tc.includeOutcomes {
				model.RequestDemand.Traffic.OutcomesMetric = "outcomes_total"
			}
			slot := now.Add(-demandPublicationDelay).UTC().Truncate(30 * time.Second)
			from := slot.Add(-30 * time.Second)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				metric := strings.SplitN(request.URL.Query().Get("query"), "{", 2)[0]
				rate := 120.
				switch metric {
				case "finished_total":
					rate = tc.work
				case "errors_total":
					rate = tc.errors
				case "outcomes_total":
					rate = 101
				}
				var values [][]any
				for i := 0; i <= 15; i++ {
					values = append(values, []any{from.Add(time.Duration(i*2) * time.Second).Unix(), fmt.Sprint(float64(i*2) * rate)})
				}
				json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{map[string]any{"metric": map[string]string{"namespace": "a", "pod": "app-pod", "pod_uid": "pod-a"}, "values": values}}}})
			}))
			defer server.Close()
			r.TrafficPrometheusURL = server.URL
			r.Gatherer = &demandOutcomeAccountingGatherer{now: now}
			deployment := &appsv1.Deployment{}
			if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "a", Name: "app"}, deployment); err != nil {
				t.Fatal(err)
			}
			raw, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(deployment)
			identity := demandHash([]interface{}{string(owner.UID), scaleTargetIdentity(owner, scale), model, raw["spec"].(map[string]interface{})["template"], demandMeasurementSource, server.URL})
			cm, state, err := r.loadDemandState(ctx, owner, model, identity)
			if err != nil {
				t.Fatal(err)
			}
			state.CreatedAt, state.LastSlot = from.Add(-time.Hour), from
			if err := r.saveDemandState(ctx, cm, state); err != nil {
				t.Fatal(err)
			}
			request, target := int64(1000), int32(60)
			r.processRequestDemand(ctx, owner, model, scale, cpuMetricSnapshot{RequestPerPodMillicores: &request, TargetUtilizationPercentage: &target}, &api.ModelStatus{}, nil)
			_, published, err := r.loadDemandState(ctx, owner, model, identity)
			if err != nil || len(published.Accounting) != 1 {
				t.Fatalf("accounting evidence not acquired: %+v %v", published.Accounting, err)
			}
			account := published.Accounting[0].Window
			contaminated := account.Reason == "failed requests contaminate completed-work accounting"
			if contaminated != tc.wantContaminated || account.Qualified == tc.wantContaminated {
				t.Fatalf("processed-work contamination guard changed: %+v", account)
			}
			fraction := published.Observations[0].Features["traffic_error_rate"]
			if tc.includeOutcomes && (fraction.Value == nil || *fraction.Value >= .01) {
				t.Fatalf("test did not distinguish all outcomes from processing work: %+v", fraction)
			}
			if !tc.includeOutcomes && fraction.Value != nil {
				t.Fatal("unconfigured all-outcome fraction was fabricated")
			}
		})
	}
}
