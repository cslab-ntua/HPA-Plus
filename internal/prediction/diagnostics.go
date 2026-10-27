package prediction

import (
	"crypto/sha256"
	"fmt"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

// DiagnosticScope separates resource UID, generation, model, history epoch and
// target identity using the controller-owned session key. It is never a path.
func DiagnosticScope(model *api.Model) string {
	if model.SessionID == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(model.SessionID)))
}
