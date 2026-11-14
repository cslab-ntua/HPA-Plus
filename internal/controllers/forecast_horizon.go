package controllers

import (
	"math"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// forecastHorizon is how far the model must forecast for HPA+ to order pods a
// lead ahead of any rise. When the order goes out, the newest forecast was built
// on a window that ended at least lead + delay earlier, and it must still reach
// the window where the rise starts and the window after it, which places the
// rise inside its window (splitRisingWindows). With W the window length:
//
//	H = W × (⌈(lead + delay) ÷ W⌉ + 2)
//
// It applies while the lead is measured (scaleUpLeadFromStartup and an observed
// pod start-up). The configured horizon stays when it is longer.
func forecastHorizon(configured, window time.Duration, model *api.Model, pods []corev1.Pod, events []demandForecastEvent) time.Duration {
	if window <= 0 || model.PredictivePolicy == nil || !model.PredictivePolicy.ScaleUpLeadFromStartup {
		return configured
	}
	startup, ok := observedStartup(pods)
	if !ok {
		return configured
	}
	lead := time.Duration(math.Ceil((startup+startupLeadMargin).Seconds())) * time.Second
	windows := int64(math.Ceil(float64(lead+forecastDelay(events, window))/float64(window))) + 2
	if rule := time.Duration(windows) * window; rule > configured {
		return rule
	}
	return configured
}

// forecastDelay is the longest time the recent forecasts took to appear after
// the window they were built on, or half a window before there is one.
func forecastDelay(events []demandForecastEvent, window time.Duration) time.Duration {
	longest, found := time.Duration(0), false
	for _, e := range events {
		if e.AvailableAt.IsZero() || e.ConsumedAt.IsZero() {
			continue
		}
		if delay := e.AvailableAt.Sub(e.ConsumedAt); delay > 0 && delay < 10*window && (!found || delay > longest) {
			longest, found = delay, true
		}
	}
	if !found {
		return window / 2
	}
	return longest
}

// withLookAhead returns a copy of the model that forecasts horizon ahead.
func withLookAhead(model *api.Model, horizon time.Duration) *api.Model {
	millis := int(horizon / time.Millisecond)
	out := *model
	switch model.Type {
	case api.TypeLinear:
		if model.Linear != nil {
			copied := *model.Linear
			copied.LookAhead = millis
			out.Linear = &copied
		}
	case api.TypeArima:
		if model.Arima != nil {
			copied := *model.Arima
			copied.LookAhead = millis
			out.Arima = &copied
		}
	case api.TypeXGBoost:
		if model.XGBoost != nil {
			copied := *model.XGBoost
			copied.LookAhead = millis
			out.XGBoost = &copied
		}
	case api.TypeLightGBM:
		if model.LightGBM != nil {
			copied := *model.LightGBM
			copied.LookAhead = millis
			out.LightGBM = &copied
		}
	case api.TypeToto:
		if model.Toto != nil {
			copied := *model.Toto
			copied.LookAhead = millis
			out.Toto = &copied
		}
	case api.TypeTiRex:
		if model.TiRex != nil {
			copied := *model.TiRex
			copied.LookAhead = millis
			out.TiRex = &copied
		}
	case api.TypeSeasonalNaive:
		if model.SeasonalNaive != nil {
			copied := *model.SeasonalNaive
			copied.LookAhead = millis
			out.SeasonalNaive = &copied
		}
	case api.TypeHoltWinters:
		if model.HoltWinters != nil {
			copied := *model.HoltWinters
			copied.LookAhead = &millis
			out.HoltWinters = &copied
		}
	}
	return &out
}
