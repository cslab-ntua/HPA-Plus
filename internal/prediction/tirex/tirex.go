package tirex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	hpaplusv1alpha1 "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"
	"github.com/cslab-ntua/HPA-Plus/internal/prediction"
	"k8s.io/apimachinery/pkg/util/uuid"
)

const (
	defaultTimeout = hpaplusv1alpha1.DefaultCalculationTimeoutMillis
	// DefaultQuantileLevel is the highest quantile TiRex-2 predicts.
	DefaultQuantileLevel = 0.9
)

const workerPath = "algorithms/tirex/tirex.py"

// SessionRunner keeps one long-lived worker per session, so the model is loaded once.
type SessionRunner interface {
	RunSessionWithValue(sessionID, algorithmPath, value string, timeout int) (string, error)
	ResetSession(sessionID string) error
}

type forecastConfig struct {
	LookAhead        int     `json:"lookAhead"`
	QuantileLevel    float64 `json:"quantileLevel"`
	ReturnTrajectory bool    `json:"returnTrajectory,omitempty"`
}

type forecastRequest struct {
	ProtocolVersion    int                                 `json:"protocolVersion"`
	DiagnosticsVersion int                                 `json:"diagnosticsVersion,omitempty"`
	Unit               string                              `json:"unit"`
	RequestID          string                              `json:"requestId"`
	Action             string                              `json:"action"`
	Config             forecastConfig                      `json:"config"`
	ReplicaHistory     []hpaplusv1alpha1.TimestampedSample `json:"replicaHistory"`
}

// Predict forecasts CPU demand with the pretrained TiRex-2 model.
type Predict struct {
	Runner SessionRunner

	mu       sync.Mutex
	sessions map[string]bool
}

func (p *Predict) GetPrediction(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (int32, error) {
	result, err := p.GetPredictionResult(model, replicaHistory)
	if err != nil {
		return 0, err
	}
	return result.Replicas, nil
}

func (p *Predict) GetPredictionResult(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (prediction.Result, error) {
	return p.GetPredictionResultContext(context.Background(), model, replicaHistory)
}

func (p *Predict) GetPredictionResultContext(ctx context.Context, model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) (prediction.Result, error) {
	return prediction.PredictCPU(ctx, p, model, replicaHistory)
}

func (p *Predict) AcceptedObservations() []prediction.MetricKind {
	return []prediction.MetricKind{prediction.CPUKind()}
}

func (p *Predict) ForecastDemand(ctx context.Context, request prediction.DemandRequest) (prediction.DemandResult, error) {
	if err := prediction.ValidateDemandRequest(request, p.AcceptedObservations()); err != nil {
		return prediction.DemandResult{}, err
	}
	model, replicaHistory := request.Model, request.History

	ctx, cancel, contextErr := prediction.ModelContext(ctx, model, defaultTimeout)
	if contextErr != nil {
		return prediction.DemandResult{}, contextErr
	}
	defer cancel()
	if err := ctx.Err(); err != nil {
		return prediction.DemandResult{}, err
	}
	if model.TiRex == nil {
		return prediction.DemandResult{}, errors.New("no TiRex configuration provided for model")
	}
	if p.Runner == nil {
		return prediction.DemandResult{}, errors.New("TiRex needs a persistent worker runner")
	}

	history := prediction.FilterCPUHistory(replicaHistory)
	if len(history) < 2 {
		return prediction.DemandResult{}, prediction.ErrInsufficientHistory
	}

	quantile := DefaultQuantileLevel
	if model.TiRex.QuantileLevel != nil {
		quantile = *model.TiRex.QuantileLevel
	}
	req := forecastRequest{
		ProtocolVersion: algorithm.CPUProtocolVersion,
		Unit:            algorithm.CPUProtocolUnit,
		RequestID:       string(uuid.NewUUID()),
		Action:          "forecast",
		Config:          forecastConfig{LookAhead: model.TiRex.LookAhead, QuantileLevel: quantile, ReturnTrajectory: model.PredictivePolicy != nil},
		ReplicaHistory:  history,
	}
	if algorithm.WantsForecastDiagnostics(ctx) {
		req.DiagnosticsVersion = 1
	}
	body, err := json.Marshal(req)
	if err != nil {
		return prediction.DemandResult{}, err
	}

	timeout := defaultTimeout
	if model.CalculationTimeout != nil {
		timeout = *model.CalculationTimeout
	}
	sessionID := sessionKey(model)
	p.remember(sessionID)

	ctx, trajectory := algorithm.WithForecastTrajectory(ctx)
	raw, err := algorithm.RunSession(ctx, p.Runner, sessionID, workerPath, string(body), timeout)
	if err != nil {
		return prediction.DemandResult{}, err
	}
	usage, err := algorithm.DecodeCPUForecastResponseContext(ctx, raw, req.RequestID)
	if err != nil {
		return prediction.DemandResult{}, err
	}
	return prediction.CompleteCPUForecast(model, prediction.DemandResult{
		Trajectory:    trajectory.Last(),
		Demand:        prediction.Demand{Kind: request.Observation.Kind, Target: request.Observation.Target, Value: usage},
		ConsumedUntil: prediction.LatestTimestamp(history),
	})
}

// PruneHistory keeps the configured number of CPU samples as model context.
func (p *Predict) PruneHistory(model *hpaplusv1alpha1.Model, replicaHistory []hpaplusv1alpha1.TimestampedSample) ([]hpaplusv1alpha1.TimestampedSample, error) {
	if model.TiRex == nil {
		return nil, errors.New("no TiRex configuration provided for model")
	}
	if model.TiRex.HistorySize < 0 {
		return nil, errors.New("historySize must be nonnegative for pruning")
	}
	return prediction.PruneCPUHistory(replicaHistory, model.TiRex.HistorySize), nil
}

func (p *Predict) GetType() string {
	return hpaplusv1alpha1.TypeTiRex
}

// PrepareAdaptation needs no refit: the pretrained model reads the newest history on every forecast.
func (p *Predict) PrepareAdaptation(ctx context.Context, model *hpaplusv1alpha1.Model) (string, error) {
	return "Refit", ctx.Err()
}

// SyncSessions stops the workers of models that are gone or whose history epoch changed.
func (p *Predict) SyncSessions(prefix string, active map[string]bool) {
	p.mu.Lock()
	var removed []string
	for id := range p.sessions {
		if strings.HasPrefix(id, prefix) && !active[id] {
			delete(p.sessions, id)
			removed = append(removed, id)
		}
	}
	p.mu.Unlock()
	if p.Runner != nil {
		for _, id := range removed {
			_ = p.Runner.ResetSession(id)
		}
	}
}

func (p *Predict) remember(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions == nil {
		p.sessions = map[string]bool{}
	}
	p.sessions[id] = true
}

func sessionKey(model *hpaplusv1alpha1.Model) string {
	if model.SessionID != "" {
		return model.SessionID
	}
	return model.Name
}
