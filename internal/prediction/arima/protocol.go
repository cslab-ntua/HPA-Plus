package arima

import (
	"github.com/cslab-ntua/HPA-Plus/internal/algorithm"

	"k8s.io/apimachinery/pkg/util/uuid"
)

const workerProtocolVersion = algorithm.CPUProtocolVersion
const workerProtocolUnit = algorithm.CPUProtocolUnit

func versionedRequest(request incrementalRequest) incrementalRequest {
	request.ProtocolVersion = workerProtocolVersion
	request.Unit = workerProtocolUnit
	request.RequestID = string(uuid.NewUUID())
	return request
}

func decodeIncrementalResponse(raw, requestID string) (int64, error) {
	return algorithm.DecodeCPUForecastResponse(raw, requestID)
}
