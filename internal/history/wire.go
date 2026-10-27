package history

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

// validateHistoryWire runs after strict typed decoding. Go scalar zero values
// cannot distinguish an explicit zero from a missing/null value. Require wire
// presence wherever that distinction could alter a scale decision or session
// identity. Other invalid values remain subject to the existing semantic checks.
func validateHistoryWire(raw []byte, data *api.HPAPlusData) error {
	var framing struct {
		SchemaVersion  json.RawMessage `json:"schemaVersion"`
		PendingScale   json.RawMessage `json:"pendingScale"`
		ModelHistories map[string]struct {
			HistoryEpoch     json.RawMessage `json:"historyEpoch"`
			CachedPrediction json.RawMessage `json:"cachedPrediction"`
		} `json:"modelHistories"`
	}
	if err := json.Unmarshal(raw, &framing); err != nil {
		return fmt.Errorf("decode history framing: %w", err)
	}
	if isJSONNull(framing.SchemaVersion) {
		return fmt.Errorf("history schemaVersion cannot be null")
	}
	for name, history := range framing.ModelHistories {
		// Omitted epochs are legacy epoch zero; explicit null is ambiguous.
		if isJSONNull(history.HistoryEpoch) || data.ModelHistories[name].HistoryEpoch < 0 {
			return fmt.Errorf("model %q historyEpoch must be a nonnegative integer when present", name)
		}
		if len(history.CachedPrediction) != 0 && !isJSONNull(history.CachedPrediction) {
			fields, err := wireFields(history.CachedPrediction)
			if err != nil {
				return err
			}
			if trajectory, ok := fields["trajectory"]; ok && !isJSONNull(trajectory) {
				if _, err := algorithm.DecodeForecastTrajectory(trajectory); err != nil {
					return fmt.Errorf("model %q trajectory: %w", name, err)
				}
			}
			if err := requireWireValue(fields, "replicas", false); err != nil {
				return fmt.Errorf("model %q cached prediction: %w", name, err)
			}
		}
	}
	if data.PendingScale == nil {
		return nil
	}
	fields, err := wireFields(framing.PendingScale)
	if err != nil {
		return err
	}
	for _, name := range []string{"id", "ownerUID", "target", "targetUID", "resourceVersion", "previousDesired",
		"desired", "observed", "time", "scaleUpRecommendations", "scaleDownRecommendations"} {
		nullable := name == "scaleUpRecommendations" || name == "scaleDownRecommendations"
		if err := requireWireValue(fields, name, nullable); err != nil {
			return fmt.Errorf("pending scale: %w", err)
		}
		if !nullable {
			continue
		}
		var rows []json.RawMessage
		if err := json.Unmarshal(fields[name], &rows); err != nil {
			return err
		}
		for index, row := range rows {
			recommendation, err := wireFields(row)
			if err != nil {
				return err
			}
			if err := requireWireValue(recommendation, "replicas", false); err != nil {
				return fmt.Errorf("pending scale %s row %d: %w", name, index, err)
			}
		}
	}
	return nil
}

func wireFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("decode history fields: %w", err)
	}
	return fields, nil
}

func requireWireValue(fields map[string]json.RawMessage, name string, nullable bool) error {
	value, present := fields[name]
	if !present || (!nullable && isJSONNull(value)) {
		return fmt.Errorf("field %s is missing or null", name)
	}
	return nil
}

func isJSONNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }
