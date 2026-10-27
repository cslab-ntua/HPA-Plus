package history

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	"math"
	strictjson "sigs.k8s.io/json"
	"strconv"
	"strings"
)

const (
	MaxAllocatedBytes = 16 << 20
	ChunkBytes        = 512 << 10
	RootLabel         = "hpa.plus/history-root"
	FenceKey          = "hpa.plus/history-fence"
)

type ChunkReference struct {
	Name   string `json:"name"`
	UID    string `json:"uid"`
	Digest string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}
type Manifest struct {
	SchemaVersion int                `json:"schemaVersion"`
	Chunks        []ChunkReference   `json:"stateChunks"`
	PendingScale  *api.ScaleDecision `json:"pendingScale,omitempty"`
}

func OwnedConfigMap(cm *corev1.ConfigMap, owner *api.HPAPlus) bool {
	if cm == nil || owner == nil || cm.Namespace != owner.Namespace {
		return false
	}
	for _, ref := range cm.OwnerReferences {
		if ref.UID == owner.UID && ref.Name == owner.Name && ref.Kind == "HPAPlus" && ref.APIVersion == api.GroupVersion.String() {
			return true
		}
	}
	return false
}
func ChunkPrefix(root *corev1.ConfigMap) string {
	sum := sha256.Sum256([]byte(root.UID))
	return "hpa-hist-" + hex.EncodeToString(sum[:6]) + "-"
}
func FenceOf(cm *corev1.ConfigMap) (uint64, error) {
	value := cm.Annotations[FenceKey]
	if value == "" {
		return 0, nil
	}
	fence, err := strconv.ParseUint(value, 10, 64)
	if err != nil || fence == math.MaxUint64 {
		return 0, fmt.Errorf("invalid or exhausted history fence")
	}
	return fence, nil
}
func DecodeManifest(payload string, root *corev1.ConfigMap) (*Manifest, error) {
	if root.UID == "" {
		return nil, fmt.Errorf("history root UID is missing")
	}
	var manifest Manifest
	issues, err := strictjson.UnmarshalStrict([]byte(payload), &manifest, strictjson.DisallowDuplicateFields, strictjson.DisallowUnknownFields)
	if err != nil {
		return nil, err
	}
	if len(issues) > 0 {
		return nil, issues[0]
	}
	if manifest.SchemaVersion != 2 || len(manifest.Chunks) == 0 || len(manifest.Chunks) > MaxAllocatedBytes/ChunkBytes {
		return nil, fmt.Errorf("invalid history manifest version/chunk count")
	}
	total := 0
	names := map[string]bool{}
	for _, ref := range manifest.Chunks {
		hash, err := hex.DecodeString(ref.Digest)
		if !strings.HasPrefix(ref.Name, ChunkPrefix(root)) || len(kvalidation.IsDNS1123Label(ref.Name)) != 0 || ref.UID == "" || names[ref.Name] || ref.Bytes < 1 || ref.Bytes > ChunkBytes || err != nil || len(hash) != 32 {
			return nil, fmt.Errorf("invalid history chunk reference")
		}
		names[ref.Name] = true
		total += ref.Bytes
	}
	if total > MaxAllocatedBytes {
		return nil, fmt.Errorf("history manifest exceeds state limit")
	}
	// Validate raw journal fields before JSON zero values can become scale intent.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &fields); err != nil {
		return nil, err
	}
	if raw, ok := fields["pendingScale"]; ok {
		envelope := `{"schemaVersion":1,"modelHistories":{},"pendingScale":` + string(raw) + `}`
		data, err := Decode(envelope)
		if err != nil {
			return nil, err
		}
		manifest.PendingScale = data.PendingScale
	}
	if _, err := FenceOf(root); err != nil {
		return nil, err
	}
	return &manifest, nil
}
