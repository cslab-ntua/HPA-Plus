package worker

import (
	"encoding/json"
	"testing"
)

func TestRequestDemandHasSeparateTypedRunProtocol(t *testing.T) {
	r := request{Version: 1, Operation: "run", Script: "algorithms/request_demand.py", Payload: json.RawMessage(`{"protocol":"request-demand-v1"}`), TimeoutMS: 10000}
	if err := r.validate(); err != nil {
		t.Fatal(err)
	}
	r.Operation = "forecast"
	if err := r.validate(); err == nil {
		t.Fatal("accepted request-demand output in CPU-only forecast protocol")
	}
}
