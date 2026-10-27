package algorithm

import (
	"context"
	"os"
	ctrlLog "sigs.k8s.io/controller-runtime/pkg/log"
	"time"
)

// FeatureDiagnosticJanitor expires opt-in feature dumps even when no model runs.
// Cleanup failures are advisory and never affect forecast availability.
type FeatureDiagnosticJanitor struct {
	runner   ContextRunner
	interval time.Duration
}

func NewFeatureDiagnosticJanitor() *FeatureDiagnosticJanitor {
	j := &FeatureDiagnosticJanitor{interval: time.Minute}
	if os.Getenv("HPAPLUS_FEATURE_DUMP_DIR") != "" {
		runner := NewAlgorithmPython()
		runner.MaxMemoryMiB = 64
		j.runner = runner
	}
	return j
}
func (*FeatureDiagnosticJanitor) NeedLeaderElection() bool { return false }
func (j *FeatureDiagnosticJanitor) Start(ctx context.Context) error {
	if j.runner == nil {
		return nil
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return nil
		}
		if _, err := j.runner.RunAlgorithmWithValueContext(ctx, "algorithms/feature_diagnostics.py", "", 5000); err != nil && ctx.Err() == nil {
			ctrlLog.FromContext(ctx).V(1).Info("Feature diagnostic cleanup failed")
		}
		timer.Reset(j.interval)
	}
}
