package prediction

import (
	"context"
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

// AdaptationProvider is optional. Adapters decide how to refit/reset while
// preserving their configured training requirements. Unsupported adapters keep
// forecasting normally and explicitly report that adaptation is unsupported.
type AdaptationProvider interface {
	PrepareAdaptation(context.Context, *api.Model) (string, error)
}

func (m *ModelPredict) PrepareAdaptation(ctx context.Context, model *api.Model) (string, error) {
	for _, p := range m.Predicters {
		if p.GetType() == model.Type {
			if a, ok := p.(AdaptationProvider); ok {
				return a.PrepareAdaptation(ctx, model)
			}
			break
		}
	}
	return "Unsupported", nil
}
