package decision

import (
	"testing"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
)

// A rule that keeps the current pods for one cycle must not fill the
// scale-down window with the current count, or no pod is ever removed.
func TestScaleDownWindowStoresRecommendationNotHold(t *testing.T) {
	window := int32(120)
	spec := api.HPAPlusSpec{MaxReplicas: 8, Models: []api.Model{{Name: "demand", RequestDemand: &api.RequestDemandConfig{}}},
		Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{ScaleDown: &autoscalingv2.HPAScalingRules{StabilizationWindowSeconds: &window}}}
	start := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	run := func(windowPredictions bool) int32 {
		status := api.HPAPlusStatus{}
		var desired int32
		// The rule holds 5 pods once per 30 s slot while it asks for 2.
		for step := 0; step < 24; step++ {
			now := start.Add(time.Duration(step) * 5 * time.Second)
			held := step%6 == 0
			prediction := int32(2)
			if held {
				prediction = 5
			}
			in := Input{Spec: spec, Status: status, Now: now, Requested: 5, Baseline: 8, Predictions: []int32{prediction}}
			if windowPredictions {
				in.WindowPredictions = []int32{2}
			}
			p, err := Plan(in)
			if err != nil {
				t.Fatal(err)
			}
			if held && p.Desired != 5 {
				t.Fatalf("step %d: the hold must keep 5 pods in its own cycle: %+v", step, p)
			}
			if last := p.ScaleDownRecommendations[len(p.ScaleDownRecommendations)-1].Replicas; windowPredictions && last != 2 {
				t.Fatalf("step %d: the window stored %d, want the recommendation 2", step, last)
			}
			status.ScaleDownReplicaHistory = p.ScaleDownRecommendations
			desired = p.Desired
		}
		return desired
	}
	if got := run(false); got != 5 {
		t.Fatalf("without window predictions the held count stays in the window: got %d", got)
	}
	// At 115 s, 25 s after the last hold, the window holds only the rule's 2,
	// so the pods may go.
	if got := run(true); got >= 5 {
		t.Fatalf("held counts kept the window at 5: got %d", got)
	}
}

func TestWindowPredictionsNeedMatchingLength(t *testing.T) {
	in := Input{Spec: api.HPAPlusSpec{MaxReplicas: 8, Models: []api.Model{{RequestDemand: &api.RequestDemandConfig{}}}},
		Now: time.Now(), Requested: 4, Predictions: []int32{4, 3}, WindowPredictions: []int32{1}}
	p, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if last := p.ScaleDownRecommendations[len(p.ScaleDownRecommendations)-1].Replicas; last != p.Guarded {
		t.Fatalf("a mismatched list must store the decided count: stored %d, guarded %d", last, p.Guarded)
	}
}
