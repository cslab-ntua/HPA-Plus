package history

import (
	"crypto/sha256"
	"fmt"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/types"
)

// TargetIdentity binds training observations to a particular workload incarnation.
func TargetIdentity(ref autoscalingv2.CrossVersionObjectReference, uid types.UID) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(ref.APIVersion+"\x00"+ref.Kind+"\x00"+ref.Name+"\x00"+string(uid))))
}
