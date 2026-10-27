package controllers

import (
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/history"
	"time"
)

func validateStoredHistory(h api.ModelHistory, now time.Time) error {
	return history.ValidateWindow(h, now)
}
func resetTrainingHistory(h *api.ModelHistory) error { return history.ResetWindow(h) }
