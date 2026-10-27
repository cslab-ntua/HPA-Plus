package controllers

import (
	"fmt"
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
)

func modelSessionID(instance *api.HPAPlus, model string) string {
	return fmt.Sprintf("%s/%s/%s/%d/%s", instance.Namespace, instance.Name, instance.UID, instance.Generation, model)
}

func modelHistorySessionID(instance *api.HPAPlus, model string, epoch int64, targetIdentity string) string {
	return fmt.Sprintf("%s/%d/%s", modelSessionID(instance, model), epoch, targetIdentity)
}

func (r *HPAPlusReconciler) syncModelSessions(namespace, name string, instance *api.HPAPlus, histories map[string]api.ModelHistory, targetIdentity string) {
	active := map[string]bool{}
	if instance != nil && instance.DeletionTimestamp == nil {
		for _, model := range instance.Spec.Models {
			if model.Type == api.TypeArima && model.Arima != nil && model.Arima.IncrementalUpdates != nil && *model.Arima.IncrementalUpdates {
				active[modelHistorySessionID(instance, model.Name, histories[model.Name].HistoryEpoch, targetIdentity)] = true
			}
		}
	}
	r.modelRuntime().SyncSessions(namespace+"/"+name+"/", active)
}

func (r *HPAPlusReconciler) modelRuntime() prediction.Runtime {
	if r.Runtime != nil {
		return r.Runtime
	}
	return prediction.Adapter{Predictor: r.Predicter}
}
