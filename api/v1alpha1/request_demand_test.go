package v1alpha1

import "testing"

func TestDemandModelCopyDoesNotAliasConfiguration(t *testing.T) {
	original := Model{RequestDemand: &RequestDemandConfig{Traffic: TrafficProviderConfig{AdditionalLabels: map[string]string{"reporter": "destination"}}, ExtraMetrics: []DemandExtraMetric{{Name: "queue", Unit: "requests"}}}}
	copied := original.DeepCopy()
	copied.RequestDemand.Traffic.AdditionalLabels["reporter"] = "source"
	copied.RequestDemand.ExtraMetrics[0].Unit = "bytes"
	if original.RequestDemand.Traffic.AdditionalLabels["reporter"] != "destination" || original.RequestDemand.ExtraMetrics[0].Unit != "requests" {
		t.Fatal("request-demand configuration aliases cached object")
	}
}
