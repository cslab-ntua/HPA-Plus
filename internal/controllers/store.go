package controllers

import (
	"context"
	"fmt"
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/history"
	"github.com/cslab-ntua/HPA-Plus/internal/observability"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ConfigMapStore adapts legacy single-object persistence to a logical CAS store.
// Control-loop mutations only see history.Snapshot; Kubernetes objects stay here.
type ConfigMapStore struct {
	Reader    client.Reader
	Client    client.Client
	Telemetry *observability.Metrics
}

func (s *ConfigMapStore) Load(ctx context.Context, owner *api.HPAPlus) (*history.Snapshot, error) {
	cm, data, err := s.getOrCreateConfigMapData(ctx, owner)
	if err != nil {
		return nil, err
	}
	return configMapSnapshot(cm, data)
}
func configMapSnapshot(cm *corev1.ConfigMap, data *api.HPAPlusData) (*history.Snapshot, error) {
	current := *data
	encoded, err := encodeHistory(&current)
	if err != nil {
		return nil, err
	}
	extra := configMapDataBytes(cm) - len(cm.Data[configMapDataKey])
	state, journal, err := historyBudgetParts(&current, len(encoded), extra)
	if err != nil {
		return nil, err
	}
	return &history.Snapshot{Data: data, Revision: cm, UnrelatedBytes: extra, StateBytes: state, JournalBytes: journal, StateBudgetBytes: maxHistoryStateBytes, JournalBudgetBytes: maxScaleJournalBytes}, nil
}
func (s *ConfigMapStore) Commit(ctx context.Context, snapshot *history.Snapshot, data *api.HPAPlusData) error {
	cm, ok := snapshot.Revision.(*corev1.ConfigMap)
	if !ok {
		return fmt.Errorf("invalid ConfigMap history revision")
	}
	if snapshot.Validate != nil {
		if err := snapshot.Validate(); err != nil {
			return err
		}
	}
	current := *data
	return s.updateConfigMapData(ctx, cm, &current)
}
func (r *HPAPlusReconciler) historyStore() history.Store {
	if r.HistoryStore != nil {
		return r.HistoryStore
	}
	return &ConfigMapStore{Reader: r.reader(), Client: r.Client, Telemetry: r.Telemetry}
}
func (r *HPAPlusReconciler) loadHistory(ctx context.Context, owner *api.HPAPlus) (*history.Snapshot, *api.HPAPlusData, error) {
	snapshot, err := r.historyStore().Load(ctx, owner)
	if err != nil {
		return nil, nil, err
	}
	return snapshot, snapshot.Data, nil
}

// Legacy helpers remain for existing direct-store callers. Reconciliation and
// sampling use loadHistory/history.Store and never inspect these objects.
func (r *HPAPlusReconciler) getOrCreateConfigMapData(ctx context.Context, owner *api.HPAPlus) (*corev1.ConfigMap, *api.HPAPlusData, error) {
	return (&ConfigMapStore{Reader: r.reader(), Client: r.Client, Telemetry: r.Telemetry}).getOrCreateConfigMapData(ctx, owner)
}
func (r *HPAPlusReconciler) updateConfigMapData(ctx context.Context, cm *corev1.ConfigMap, data *api.HPAPlusData) error {
	return (&ConfigMapStore{Reader: r.reader(), Client: r.Client, Telemetry: r.Telemetry}).updateConfigMapData(ctx, cm, data)
}
