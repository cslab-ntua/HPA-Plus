package prediction

import api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"

// RequiredHistorySize is the shared controller/replay warmup requirement.
func RequiredHistorySize(model *api.Model) int {
	switch model.Type {
	case api.TypeLinear:
		if model.Linear != nil {
			return model.Linear.HistorySize
		}
	case api.TypeXGBoost:
		if model.XGBoost != nil {
			return model.XGBoost.HistorySize
		}
	case api.TypeLightGBM:
		if model.LightGBM != nil {
			return model.LightGBM.HistorySize
		}
	case api.TypeArima:
		if model.Arima != nil && model.Arima.HistorySize != nil {
			return *model.Arima.HistorySize
		}
		return api.DefaultARIMAHistorySize
	case api.TypeHoltWinters:
		if model.HoltWinters != nil {
			required := model.HoltWinters.SeasonalPeriods * model.HoltWinters.StoredSeasons
			statsmodelsMin := 2 * model.HoltWinters.SeasonalPeriods
			statsmodelsHeuristic := 10 + 2*(model.HoltWinters.SeasonalPeriods/2)
			if statsmodelsMin > required {
				required = statsmodelsMin
			}
			if statsmodelsHeuristic > required {
				required = statsmodelsHeuristic
			}
			return required
		}
	}
	return 0
}
