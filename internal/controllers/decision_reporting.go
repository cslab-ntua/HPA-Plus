package controllers

import (
	"context"
	"encoding/json"
	"sort"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/observability"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type decisionTraceKey struct{}
type decisionTrace struct{ id, write string }

func decisionContext(ctx context.Context, id string) (context.Context, *decisionTrace) {
	if id == "" {
		id = string(uuid.NewUUID())
	}
	trace := &decisionTrace{id: id, write: "not_attempted"}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("decisionID", observability.Field(id)))
	return context.WithValue(ctx, decisionTraceKey{}, trace), trace
}

func resourceLogContext(ctx context.Context, instance *api.HPAPlus) context.Context {
	return log.IntoContext(ctx, log.FromContext(ctx).WithValues("resourceUID", string(instance.UID), "generation", instance.Generation,
		"targetAPIVersion", observability.Field(instance.Spec.ScaleTargetRef.APIVersion),
		"targetKind", observability.Field(instance.Spec.ScaleTargetRef.Kind), "targetName", observability.Field(instance.Spec.ScaleTargetRef.Name)))
}

type loggedCandidate struct {
	Model    string `json:"model"`
	Replicas int32  `json:"replicas"`
}

func decisionCandidates(candidates map[string]int32) ([]loggedCandidate, int) {
	names := make([]string, 0, len(candidates))
	for name := range candidates {
		names = append(names, name)
	}
	sort.Strings(names)
	omitted := 0
	if len(names) > 64 {
		omitted = len(names) - 64
		names = names[:64]
	}
	result := make([]loggedCandidate, 0, len(names))
	for _, name := range names {
		result = append(result, loggedCandidate{observability.Field(name), candidates[name]})
	}
	return result, omitted
}

// JSON escaping can expand even short model names. Bound persisted evidence by
// encoded size as well as candidate count, and report every omitted candidate.
func boundDecisionEvidence(evidence *api.DecisionEvidence) {
	for {
		raw, _ := json.Marshal(evidence)
		if len(raw) <= 16<<10 || len(evidence.Candidates)+len(evidence.Models) == 0 {
			return
		}
		if len(evidence.Models) > 0 {
			evidence.Models = evidence.Models[:len(evidence.Models)-1]
			evidence.ModelsOmitted++
			continue
		}
		evidence.Candidates = evidence.Candidates[:len(evidence.Candidates)-1]
		evidence.CandidatesOmitted++
	}
}
