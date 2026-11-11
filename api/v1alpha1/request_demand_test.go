package v1alpha1

import "testing"

func TestDemandModelCopyDoesNotAliasConfiguration(t *testing.T) {
	original := Model{RequestDemand: &RequestDemandConfig{Traffic: TrafficProviderConfig{AdditionalLabels: map[string]string{"reporter": "destination"}}}}
	copied := original.DeepCopy()
	copied.RequestDemand.Traffic.AdditionalLabels["reporter"] = "source"
	if original.RequestDemand.Traffic.AdditionalLabels["reporter"] != "destination" {
		t.Fatal("request-demand configuration aliases cached object")
	}
}
