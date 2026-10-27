package cgroup

import (
	"fmt"
	"math"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/labels"
)

func maximum(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func minimum(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Integrate sums actual measured rates only. A fresh snapshot can contain old
// historical windows; freshness of its envelope never makes those rates fresh.
func Integrate(snapshots []Snapshot, namespace string, selector labels.Selector, from, to, received time.Time) (*WorkloadObservation, error) {
	if from.IsZero() || !to.After(from) || to.Sub(from) > time.Minute || to.After(received.Add(time.Second)) {
		return nil, fmt.Errorf("invalid cgroup accounting interval")
	}
	result := &WorkloadObservation{From: from, To: to, ReceivedAt: received, Complete: true, InventoryComplete: true,
		NativeSnapshots: append([]Snapshot(nil), snapshots...)}
	seenPods := map[string]bool{}
	var expectedSeconds, coveredSeconds float64
	var memoryTotal uint64
	memoryComplete := true
	memoryCount := 0
	var memorySourceAt time.Time
	var throttled, some, full float64
	throttledComplete, someComplete, fullComplete := true, true, true
	missing := func(reason string) {
		result.Complete = false
		if len(result.MissingReasons) < 32 {
			result.MissingReasons = append(result.MissingReasons, reason)
		}
	}
	for _, snapshot := range snapshots {
		if snapshot.Version != Protocol || snapshot.Node == "" || snapshot.CollectedAt.IsZero() || snapshot.CollectedAt.After(received.Add(time.Second)) || received.Sub(snapshot.CollectedAt) > 10*time.Second || snapshot.MissingReason != "" {
			return nil, fmt.Errorf("invalid or stale cgroup snapshot")
		}
		for _, pod := range snapshot.Pods {
			if pod.Namespace != namespace || !selector.Matches(labels.Set(pod.Labels)) {
				continue
			}
			if pod.UID == "" || seenPods[pod.UID] {
				return nil, fmt.Errorf("duplicate or unidentified cgroup pod")
			}
			seenPods[pod.UID] = true
			result.Inventory = append(result.Inventory, pod)
			// Kubernetes creation is an exact lower bound for every runtime in
			// the pod. Preserve its identity without treating a future startup as
			// an unknown process during this already-closed interval.
			if !pod.CreatedAt.IsZero() && !pod.CreatedAt.Before(to) {
				continue
			}
			if len(pod.Containers) == 0 {
				result.InventoryComplete = false
				missing(pod.Name + ": runtime inventory unavailable")
				continue
			}
			var lifetimes []struct{ from, to time.Time }
			included := false
			for _, container := range pod.Containers {
				start := maximum(from, container.StartedAt)
				end := to
				if container.FinishedAt != nil {
					end = minimum(end, *container.FinishedAt)
				} else if container.EndUpperBound != nil {
					end = minimum(end, *container.EndUpperBound)
				}
				if !end.After(start) {
					continue
				}
				if container.ID == "" || container.StartedAt.IsZero() {
					result.InventoryComplete = false
					missing(pod.Name + "/" + container.Name + ": unknown running epoch")
					continue
				}
				included = true
				expectedSeconds += end.Sub(start).Seconds()
				lifetimes = append(lifetimes, struct{ from, to time.Time }{start, end})
				var previousTo time.Time
				var coverage float64
				var coveredDuration time.Duration
				for _, window := range container.Windows {
					if !window.To.After(window.From) || window.To.Sub(window.From) > 10*time.Second || window.From.Before(container.StartedAt) || !previousTo.IsZero() && window.From.Before(previousTo) || math.IsNaN(window.CPUSeconds) || math.IsInf(window.CPUSeconds, 0) || window.CPUSeconds < 0 {
						return nil, fmt.Errorf("invalid cgroup counter window")
					}
					previousTo = window.To
					left, right := maximum(start, window.From), minimum(end, window.To)
					if !right.After(left) {
						continue
					}
					seconds := right.Sub(left).Seconds()
					fraction := seconds / window.To.Sub(window.From).Seconds()
					if window.To.After(result.SourceAt) {
						result.SourceAt = window.To
					}
					coverage += seconds
					coveredDuration += right.Sub(left)
					result.CPUSeconds += window.CPUSeconds * fraction
					if !left.Equal(window.From) || !right.Equal(window.To) {
						result.BoundaryAveraged = true
					}
					if window.ThrottledSeconds != nil {
						throttled += *window.ThrottledSeconds * fraction
					} else {
						throttledComplete = false
					}
					if window.PressureSomeSeconds != nil {
						some += *window.PressureSomeSeconds * fraction
					} else {
						someComplete = false
					}
					if window.PressureFullSeconds != nil {
						full += *window.PressureFullSeconds * fraction
					} else {
						fullComplete = false
					}
				}
				coveredSeconds += coverage
				if coveredDuration < end.Sub(start) {
					missing(pod.Name + "/" + container.Name + ": incomplete counter windows")
				}
			}
			memory, source, complete := memoryPoint(pod.Containers, to)
			if !complete {
				memoryComplete = false
			} else if memory != nil {
				if math.MaxUint64-memoryTotal < *memory {
					return nil, fmt.Errorf("memory sum overflow")
				}
				memoryTotal += *memory
				memoryCount++
				if memorySourceAt.IsZero() || source.Before(memorySourceAt) {
					memorySourceAt = source
				}
			}
			if included {
				result.PodCount++
				// One pod consumes one unit of background capacity while any of its
				// measured container epochs runs. Restart gaps are not running time.
				sort.Slice(lifetimes, func(i, j int) bool { return lifetimes[i].from.Before(lifetimes[j].from) })
				left, right := lifetimes[0].from, lifetimes[0].to
				for _, life := range lifetimes[1:] {
					if life.from.After(right) {
						result.RunningPodSeconds += right.Sub(left).Seconds()
						left, right = life.from, life.to
					} else if life.to.After(right) {
						right = life.to
					}
				}
				result.RunningPodSeconds += right.Sub(left).Seconds()
				result.Pods = append(result.Pods, pod)
			}
		}
	}
	if expectedSeconds > 0 {
		result.Coverage = math.Min(1, coveredSeconds/expectedSeconds)
	} else {
		missing("no running container accounting")
	}
	if memoryComplete && memoryCount > 0 {
		result.MemoryBytes = &memoryTotal
		result.MemorySourceAt = memorySourceAt
	}
	if throttledComplete && coveredSeconds > 0 {
		result.ThrottledSeconds = &throttled
	}
	if someComplete && coveredSeconds > 0 {
		result.PressureSomeSeconds = &some
	}
	if fullComplete && coveredSeconds > 0 {
		result.PressureFullSeconds = &full
	}
	return result, nil
}

// memoryPoint is a point measurement, separate from interval CPU accounting.
// Only epochs known alive at the query instant contribute. An uncertain end or
// missing newest reading never revives an older memory value as a fresh point.
func memoryPoint(containers []Container, at time.Time) (*uint64, time.Time, bool) {
	var total uint64
	var oldest time.Time
	count := 0
	for _, container := range containers {
		if container.ID == "" || container.StartedAt.IsZero() {
			return nil, time.Time{}, false
		}
		if container.StartedAt.After(at) {
			continue
		}
		if container.FinishedAt != nil && !container.FinishedAt.After(at) {
			continue
		}
		if container.FinishedAt == nil && container.EndUpperBound != nil && !container.EndUpperBound.After(at) {
			continue
		}
		// No termination report: a known cgroup read at/after the query instant
		// is required before declaring this epoch alive at that instant.
		if container.FinishedAt == nil && (len(container.Windows) == 0 || container.Windows[len(container.Windows)-1].To.Before(at)) {
			return nil, time.Time{}, false
		}
		var value *uint64
		var source time.Time
		for _, window := range container.Windows {
			observed := window.MemorySourceAt
			if observed.IsZero() {
				observed = window.To
			}
			if observed.After(at) || observed.Before(container.StartedAt) {
				continue
			}
			if source.IsZero() || observed.After(source) {
				source = observed
				value = window.MemoryBytes
			}
		}
		if value == nil || source.IsZero() || at.Sub(source) > 10*time.Second {
			return nil, time.Time{}, false
		}
		if math.MaxUint64-total < *value {
			return nil, time.Time{}, false
		}
		total += *value
		count++
		if oldest.IsZero() || source.Before(oldest) {
			oldest = source
		}
	}
	if count == 0 {
		return nil, time.Time{}, true
	}
	return &total, oldest, true
}
