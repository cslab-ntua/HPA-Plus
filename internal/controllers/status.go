package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *HPAPlusReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *HPAPlusReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// mutateStatus re-reads on conflicts and patches only the fields changed by mutate.
// It never transfers a decision to a replacement resource or a newer specification.
func (r *HPAPlusReconciler) mutateStatus(ctx context.Context, instance *api.HPAPlus, mutate func(*api.HPAPlusStatus)) error {
	return r.mutateStatusChecked(ctx, instance, func(status *api.HPAPlusStatus) error {
		mutate(status)
		return nil
	})
}

// mutateStatusChecked also permits a decision guard to reject a delayed retry
// before stale recommendations are persisted for later stabilization decisions.
func (r *HPAPlusReconciler) mutateStatusChecked(ctx context.Context, instance *api.HPAPlus, mutate func(*api.HPAPlusStatus) error) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		latest := &api.HPAPlus{}
		if err := r.reader().Get(ctx, client.ObjectKeyFromObject(instance), latest); err != nil {
			return err
		}
		if latest.UID != instance.UID || latest.Generation != instance.Generation || latest.DeletionTimestamp != nil {
			return fmt.Errorf("HPAPlus changed while evaluating decision")
		}
		base := latest.DeepCopy()
		if err := mutate(&latest.Status); err != nil {
			return err
		}
		if equality.Semantic.DeepEqual(base.Status, latest.Status) {
			*instance = *latest
			return nil
		}
		if err := checkStatusBudget(&base.Status, &latest.Status, maxStatusBytes); err != nil {
			r.Telemetry.ObserveStatusRejection("complete")
			return err
		}
		patch, err := requiredStatusPatch(base, latest)
		if err != nil {
			return err
		}
		if err := r.Client.Status().Patch(ctx, latest, patch); err != nil {
			return err
		}
		*instance = *latest
		r.Events.Conditions(instance, base.Status.Conditions, latest.Status.Conditions)
		return nil
	})
}

// A CRD status subresource starts absent. Typed merge-diff generation sees the
// required zero values in both Go structs and omits them, leaving an invalid
// first status patch. Include those zero values explicitly without overwriting
// nonzero fields owned by another writer. The original resourceVersion guard
// protects even unchanged zeros against a concurrent scaling/status update.
func requiredStatusPatch(base, latest *api.HPAPlus) (client.Patch, error) {
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	if latest.Status.DesiredReplicas != 0 && latest.Status.Reference != "" {
		return patch, nil
	}
	raw, err := patch.Data(latest)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	status := map[string]json.RawMessage{}
	if value := fields["status"]; len(value) != 0 {
		if err := json.Unmarshal(value, &status); err != nil {
			return nil, err
		}
	}
	if latest.Status.DesiredReplicas == 0 {
		status["desiredReplicas"] = json.RawMessage("0")
	}
	if latest.Status.Reference == "" {
		status["reference"] = json.RawMessage(`""`)
	}
	fields["status"], err = json.Marshal(status)
	if err != nil {
		return nil, err
	}
	raw, err = json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return client.RawPatch(types.MergePatchType, raw), nil
}
