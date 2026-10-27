// Package cgroup collects Linux cgroup-v2 counters without a runtime socket.
// Every CPU value describes the interval between two real counter reads.
package cgroup

import "time"

const Protocol = "hpaplus-cgroup-v1"

type Window struct {
	From                time.Time `json:"from"`
	To                  time.Time `json:"to"`
	CPUSeconds          float64   `json:"cpuSeconds"`
	MemoryBytes         *uint64   `json:"memoryBytes,omitempty"`
	MemorySourceAt      time.Time `json:"memorySourceAt,omitempty"`
	ThrottledSeconds    *float64  `json:"throttledSeconds,omitempty"`
	Periods             *uint64   `json:"periods,omitempty"`
	ThrottledPeriods    *uint64   `json:"throttledPeriods,omitempty"`
	PressureSomeSeconds *float64  `json:"pressureSomeSeconds,omitempty"`
	PressureFullSeconds *float64  `json:"pressureFullSeconds,omitempty"`
}

type Container struct {
	Name       string     `json:"name"`
	ID         string     `json:"id"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	// EndUpperBound records a real read at which a previously resolved cgroup
	// directory was absent. Its unmeasured tail remains missing; it is not an
	// exact process termination timestamp.
	EndUpperBound *time.Time `json:"endUpperBound,omitempty"`
	Windows       []Window   `json:"windows"`
	MissingReason string     `json:"missingReason,omitempty"`
}

type Pod struct {
	Namespace  string            `json:"namespace"`
	Name       string            `json:"name"`
	UID        string            `json:"uid"`
	Labels     map[string]string `json:"labels"`
	CreatedAt  time.Time         `json:"createdAt"`
	DeletedAt  *time.Time        `json:"deletedAt,omitempty"`
	Containers []Container       `json:"containers"`
}

type Snapshot struct {
	Version        string    `json:"version"`
	Node           string    `json:"node"`
	CollectedAt    time.Time `json:"collectedAt"`
	IntervalMillis int64     `json:"intervalMillis"`
	Pods           []Pod     `json:"pods"`
	MissingReason  string    `json:"missingReason,omitempty"`
}

// WorkloadObservation integrates measured windows. BoundaryAveraged means a
// boundary intersects a short counter interval: its measured average rate is
// applied only to that overlap. No rate is extrapolated across an absent read.
type WorkloadObservation struct {
	From                time.Time `json:"from"`
	To                  time.Time `json:"to"`
	SourceAt            time.Time `json:"sourceAt"`
	ReceivedAt          time.Time `json:"receivedAt"`
	CPUSeconds          float64   `json:"cpuSeconds"`
	RunningPodSeconds   float64   `json:"runningPodSeconds"`
	Coverage            float64   `json:"coverage"`
	Complete            bool      `json:"complete"`
	BoundaryAveraged    bool      `json:"boundaryAveraged"`
	MemoryBytes         *uint64   `json:"memoryBytes,omitempty"`
	MemorySourceAt      time.Time `json:"memorySourceAt,omitempty"`
	ThrottledSeconds    *float64  `json:"throttledSeconds,omitempty"`
	PressureSomeSeconds *float64  `json:"pressureSomeSeconds,omitempty"`
	PressureFullSeconds *float64  `json:"pressureFullSeconds,omitempty"`
	PodCount            int       `json:"podCount"`
	MissingReasons      []string  `json:"missingReasons,omitempty"`
	Pods                []Pod     `json:"pods"`
	// Inventory preserves all matched retained lifecycle identities, including
	// epochs known to have ended before From. Presence here is not evidence of
	// CPU coverage or running capacity in this observation.
	Inventory []Pod `json:"inventory,omitempty"`
	// InventoryComplete distinguishes trusted lifecycle population from complete
	// CPU counter support. A short counter gap can leave lifecycle identities
	// known; an unidentified runtime cannot.
	InventoryComplete bool `json:"inventoryComplete"`
	// NativeSnapshots retains the validated node batch for local exact-interval
	// accounting. It is read-only: filtered callers allocate new snapshot/pod
	// slices and never change shared labels, containers or counter windows.
	NativeSnapshots []Snapshot `json:"-"`
}
