package v1alpha1

// TimestampedReplicas is retained for Go source compatibility. Its wire encoding
// is unchanged; prediction trains on CPU demand rather than past replica counts.
// Deprecated: use TimestampedSample.
// +kubebuilder:object:generate=false
type TimestampedReplicas = TimestampedSample
