package metrics

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	gathered "github.com/jthomperoo/k8shorizmetrics/v2/metrics"
	"time"
)

// CPUSource identifies the contributing pod measurements before evaluation.
// The oldest timestamp is a conservative watermark: every contributing source
// must move beyond it before the next training sample is admitted. Missing or
// future timestamps retain legacy slot sampling unless metricMaxAge rejects them.
func CPUSource(m *gathered.Metric, now time.Time) (string, time.Time) {
	if m == nil || m.Resource == nil {
		return "", time.Time{}
	}
	stamps := map[string]string{}
	var oldest time.Time
	for name, value := range m.Resource.PodMetricsInfo {
		if m.Resource.Requests[name] <= 0 {
			continue
		}
		if value.Timestamp.IsZero() || value.Timestamp.After(now) {
			return "", time.Time{}
		}
		stamps[name] = value.Timestamp.UTC().Format(time.RFC3339Nano)
		if oldest.IsZero() || value.Timestamp.Before(oldest) {
			oldest = value.Timestamp
		}
	}
	if len(stamps) == 0 {
		return "", time.Time{}
	}
	raw, _ := json.Marshal(stamps)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), oldest
}
