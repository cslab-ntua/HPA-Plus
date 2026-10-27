package controllers

import (
	"context"
	"fmt"
	"github.com/cslab-ntua/HPA-Plus/internal/history"
	"time"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	historySchemaVersion = history.SchemaVersion
	// Kubernetes limits the combined ConfigMap data and binaryData values to 1 MiB.
	maxConfigMapDataBytes = history.MaxDataBytes
)

func encodeHistory(data *hpaplusv1alpha1.HPAPlusData) ([]byte, error) { return history.Encode(data) }
func decodeHistory(payload string) (*hpaplusv1alpha1.HPAPlusData, error) {
	return history.Decode(payload)
}

// updateConfigMapData updates the HPA+'s configmap and the data it holds
func (s *ConfigMapStore) updateConfigMapData(ctx context.Context, configMap *corev1.ConfigMap,
	hpaPlusData *hpaplusv1alpha1.HPAPlusData) (writeErr error) {
	started, outcome := time.Now(), "success"
	defer func() {
		writeErr = storageFailure("HistoryWriteFailed", writeErr)
		s.Telemetry.ObserveHistoryOperation("write", historyOperationOutcome(ctx, writeErr, outcome), time.Since(started))
	}()
	data, err := encodeHistory(hpaPlusData)
	if err != nil {
		return storageFailure("HistoryDataInvalid", fmt.Errorf("encode model history: %w", err))
	}

	newData := string(data)
	if configMap.Data != nil && configMap.Data[configMapDataKey] == newData {
		outcome = "noop"
		return nil
	}

	// Own only our payload key; preserve unrelated data and metadata. Build a
	// copy so a rejected size check does not mutate the caller's object either.
	updated := configMap.DeepCopy()
	if updated.Data == nil {
		updated.Data = map[string]string{}
	}
	updated.Data[configMapDataKey] = newData
	size := configMapDataBytes(updated)
	s.Telemetry.ObserveHistoryBytes("write", size)
	if size > maxConfigMapDataBytes {
		return storageFailure("HistoryCapacityExceeded", fmt.Errorf("history ConfigMap data is %d bytes; limit is %d; reduce model count or retained history", size, maxConfigMapDataBytes))
	}
	if err := checkHistoryBudget(configMap, hpaPlusData, len(data)); err != nil {
		return err
	}

	err = s.Client.Update(ctx, updated)
	if err != nil {
		return fmt.Errorf("failed to update config map data: %w", err)
	}

	return nil
}

func (s *ConfigMapStore) getOrCreateConfigMapData(ctx context.Context,
	instance *hpaplusv1alpha1.HPAPlus,
) (_ *corev1.ConfigMap, _ *hpaplusv1alpha1.HPAPlusData, readErr error) {
	started := time.Now()
	defer func() {
		readErr = storageFailure("HistoryReadFailed", readErr)
		s.Telemetry.ObserveHistoryOperation("read", historyOperationOutcome(ctx, readErr, "success"), time.Since(started))
	}()
	configMapName := fmt.Sprintf("hpa-plus-%s-data", instance.Name)
	configMap := &corev1.ConfigMap{}

	err := s.Reader.Get(ctx, types.NamespacedName{
		Name:      configMapName,
		Namespace: instance.Namespace,
	}, configMap)
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return nil, nil, err
		}

		configMap = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      configMapName,
				Namespace: instance.Namespace,
			},
		}
		configMap.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: hpaplusv1alpha1.GroupVersion.String(),
			Kind:       "HPAPlus",
			Name:       instance.Name,
			UID:        instance.UID,
		}})

		hpaPlusData := &hpaplusv1alpha1.HPAPlusData{
			ModelHistories: map[string]hpaplusv1alpha1.ModelHistory{},
		}
		data, err := encodeHistory(hpaPlusData)
		if err != nil {
			return nil, nil, fmt.Errorf("encode initial model history: %w", err)
		}
		configMap.Data = map[string]string{
			configMapDataKey: string(data),
		}
		createdAt := time.Now()
		s.Telemetry.ObserveHistoryBytes("create", configMapDataBytes(configMap))
		createErr := s.Client.Create(ctx, configMap)
		s.Telemetry.ObserveHistoryOperation("create", historyOperationOutcome(ctx, createErr, "success"), time.Since(createdAt))
		if err := createErr; err != nil {
			if !k8serrors.IsAlreadyExists(err) {
				return nil, nil, err
			}
			if err := s.Reader.Get(ctx, types.NamespacedName{
				Name:      configMapName,
				Namespace: instance.Namespace,
			}, configMap); err != nil {
				return nil, nil, err
			}
		} else {
			return configMap, hpaPlusData, nil
		}
	}

	s.Telemetry.ObserveHistoryBytes("read", configMapDataBytes(configMap))
	owned := false
	for _, owner := range configMap.OwnerReferences {
		if owner.Kind == "HPAPlus" && owner.APIVersion == hpaplusv1alpha1.GroupVersion.String() &&
			owner.UID == instance.UID && owner.Name == instance.Name {
			owned = true
		}
	}
	if !owned {
		return nil, nil, storageFailure("HistoryOwnershipMismatch", fmt.Errorf("history ConfigMap is not owned by this HPAPlus instance"))
	}
	hpaPlusData := &hpaplusv1alpha1.HPAPlusData{}
	if configMap.Data == nil || configMap.Data[configMapDataKey] == "" {
		hpaPlusData.ModelHistories = map[string]hpaplusv1alpha1.ModelHistory{}
		return configMap, hpaPlusData, nil
	}
	hpaPlusData, err = decodeHistory(configMap.Data[configMapDataKey])
	if err != nil {
		return nil, nil, storageFailure("HistoryDataInvalid", err)
	}

	return configMap, hpaPlusData, nil
}

func (r *HPAPlusReconciler) mutateConfigMapData(ctx context.Context,
	instance *hpaplusv1alpha1.HPAPlus,
	mutate func(*hpaplusv1alpha1.HPAPlusData) (bool, error),
) error {
	return r.mutateHistoryChecked(ctx, instance, mutate, nil)
}

func (r *HPAPlusReconciler) mutateHistoryChecked(ctx context.Context, instance *hpaplusv1alpha1.HPAPlus,
	mutate func(*hpaplusv1alpha1.HPAPlusData) (bool, error), validate func(*hpaplusv1alpha1.HPAPlusData) error) error {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		latest := &hpaplusv1alpha1.HPAPlus{}
		if err := r.reader().Get(ctx, client.ObjectKeyFromObject(instance), latest); err != nil {
			return err
		}
		if latest.UID != instance.UID || latest.Generation != instance.Generation || latest.DeletionTimestamp != nil {
			return fmt.Errorf("HPAPlus changed while updating history")
		}
		snapshot, hpaPlusData, err := r.loadHistory(ctx, instance)
		if err != nil {
			return err
		}

		changed, err := mutate(hpaPlusData)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}

		snapshot.Validate = func() error {
			current := &hpaplusv1alpha1.HPAPlus{}
			if err := r.reader().Get(ctx, client.ObjectKeyFromObject(instance), current); err != nil {
				return err
			}
			if current.UID != instance.UID || current.Generation != instance.Generation || current.DeletionTimestamp != nil {
				return fmt.Errorf("HPAPlus changed before publishing history")
			}
			if validate != nil {
				return validate(hpaPlusData)
			}
			return ctx.Err()
		}
		err = r.historyStore().Commit(ctx, snapshot, hpaPlusData)
		if err == nil {
			return nil
		}
		if !k8serrors.IsConflict(err) {
			return err
		}
		lastErr = err
	}

	if lastErr != nil {
		return lastErr
	}
	return nil
}

func configMapDataBytes(configMap *corev1.ConfigMap) int {
	size := 0
	for _, value := range configMap.Data {
		size += len(value)
	}
	for _, value := range configMap.BinaryData {
		size += len(value)
	}
	return size
}
