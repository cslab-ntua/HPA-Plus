package history

import (
	"fmt"
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	pathvalidation "k8s.io/apimachinery/pkg/api/validation/path"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"strings"
	"time"
)

func ValidateDecision(decision *api.ScaleDecision) error {
	if decision == nil {
		return fmt.Errorf("missing decision")
	}
	for _, value := range []string{decision.ID, decision.OwnerUID, decision.TargetUID, decision.ResourceVersion,
		decision.Target.Kind, decision.Target.Name} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("missing decision or resource identity")
		}
	}
	if decision.PreviousDesired < 0 || decision.Desired < 0 || decision.Observed < 0 || decision.Desired == decision.PreviousDesired {
		return fmt.Errorf("invalid replica counts")
	}
	if len(pathvalidation.IsValidPathSegmentName(decision.Target.Name)) != 0 || len(pathvalidation.IsValidPathSegmentName(decision.Target.Kind)) != 0 {
		return fmt.Errorf("invalid target reference")
	}
	if decision.Target.APIVersion != "" {
		gv, err := schema.ParseGroupVersion(decision.Target.APIVersion)
		if err != nil || len(utilvalidation.IsDNS1035Label(gv.Version)) != 0 || (gv.Group != "" && len(utilvalidation.IsDNS1123Subdomain(gv.Group)) != 0) {
			return fmt.Errorf("invalid target apiVersion")
		}
	}
	if decision.Time.IsZero() {
		return fmt.Errorf("missing decision timestamp")
	}
	for _, history := range [][]api.TimestampedSample{decision.ScaleUpRecommendations, decision.ScaleDownRecommendations} {
		for _, recommendation := range history {
			if recommendation.Time == nil || recommendation.Time.IsZero() || recommendation.Time.After(decision.Time.Time) || recommendation.Replicas < 0 {
				return fmt.Errorf("invalid stabilization recommendation")
			}
		}
	}
	return nil
}

func ValidateDecisionAt(decision *api.ScaleDecision, now time.Time) error {
	if err := ValidateDecision(decision); err != nil {
		return err
	}
	if decision.Time.After(now) {
		return fmt.Errorf("pending decision timestamp is in the future")
	}
	return nil
}
