// Package history defines the persisted history format and offline recovery rules.
package history

import (
	"bytes"
	"encoding/json"
	"fmt"
	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	strictjson "sigs.k8s.io/json"
)

const SchemaVersion = 1
const MaxDataBytes = 1 << 20

func Encode(data *hpaplusv1alpha1.HPAPlusData) ([]byte, error) {
	if data.SchemaVersion != 0 && data.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("unsupported history schema version %d", data.SchemaVersion)
	}
	data.SchemaVersion = SchemaVersion
	return json.Marshal(data)
}

func Decode(payload string) (*hpaplusv1alpha1.HPAPlusData, error) {
	return DecodeBounded(payload, MaxDataBytes)
}

// DecodeBounded decodes an assembled snapshot with an explicit backend limit.
func DecodeBounded(payload string, limit int) (*hpaplusv1alpha1.HPAPlusData, error) {
	if limit <= 0 || len(payload) > limit {
		return nil, fmt.Errorf("history payload exceeds ConfigMap data limit")
	}
	raw := bytes.TrimSpace([]byte(payload))
	if len(raw) == 0 || raw[0] != '{' {
		return nil, fmt.Errorf("history payload must be a JSON object")
	}
	data := &hpaplusv1alpha1.HPAPlusData{}
	// A permissive decode can discard a pending journal or unknown future state
	// on the next write. Reject ambiguous/unknown fields at every typed level,
	// including duplicate map keys, before handing any data to a mutator.
	strictErrors, err := strictjson.UnmarshalStrict(raw, data, strictjson.DisallowDuplicateFields, strictjson.DisallowUnknownFields)
	if err != nil {
		return nil, fmt.Errorf("decode model history: %w", err)
	}
	if len(strictErrors) != 0 {
		return nil, fmt.Errorf("decode model history: %w", strictErrors[0])
	}
	if data.SchemaVersion != 0 && data.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("unsupported history schema version %d", data.SchemaVersion)
	}
	if err := validateHistoryWire(raw, data); err != nil {
		return nil, err
	}
	if data.ModelHistories == nil {
		data.ModelHistories = map[string]hpaplusv1alpha1.ModelHistory{}
	}
	return data, nil
}
