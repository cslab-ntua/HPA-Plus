package cgroup

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const MaxPods = 2048
const MaxContainers = 32
const MaxTrackedContainers = 512
const MaxWindows = 180
const MaxSnapshotBytes = 32 << 20

type containerState struct {
	row      Container
	previous *Counters
	path     string
}
type podState struct {
	row        Pod
	containers map[string]*containerState
	lastSeen   time.Time
}

type Collector struct {
	Root, Node          string
	Interval, Retention time.Duration
	mu                  sync.RWMutex
	pods                map[string]*podState
	collectedAt         time.Time
	missing             string
}

func NewCollector(root, node string, interval, retention time.Duration) (*Collector, error) {
	if node == "" || interval < time.Second || interval > 10*time.Second || retention < time.Minute || retention > 5*time.Minute {
		return nil, fmt.Errorf("invalid cgroup collector configuration")
	}
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		return nil, fmt.Errorf("cgroup-v2 mount unavailable: %w", err)
	}
	return &Collector{Root: root, Node: node, Interval: interval, Retention: retention, pods: map[string]*podState{}}, nil
}

// Collect must be called by one sampling loop. Pod discovery is driven by the
// node-filtered Kubernetes informer, independently of workload reconciliations.
func (c *Collector) Collect(pods []*corev1.Pod, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.missing = ""
	if len(pods) > MaxPods {
		c.missing = "node pod bound exceeded"
		c.collectedAt = now
		return
	}
	seen := map[string]bool{}
	tracked := 0
	for _, pod := range c.pods {
		tracked += len(pod.containers)
	}
	for _, pod := range pods {
		if pod.Spec.NodeName != c.Node || pod.UID == "" {
			continue
		}
		uid := string(pod.UID)
		seen[uid] = true
		state := c.pods[uid]
		if state == nil {
			state = &podState{row: Pod{Namespace: pod.Namespace, Name: pod.Name, UID: uid, CreatedAt: pod.CreationTimestamp.Time}, containers: map[string]*containerState{}}
			c.pods[uid] = state
		}
		state.lastSeen = now
		state.row.Labels = pod.Labels
		if pod.DeletionTimestamp != nil {
			t := pod.DeletionTimestamp.Time
			state.row.DeletedAt = &t
		}
		statuses := append(append([]corev1.ContainerStatus{}, pod.Status.ContainerStatuses...), pod.Status.InitContainerStatuses...)
		expected := map[string]bool{}
		for _, container := range pod.Spec.Containers {
			expected[container.Name] = true
		}
		// Native sidecars appear in init statuses; ordinary running init
		// containers also consume real startup CPU and are retained explicitly.
		for _, status := range pod.Status.InitContainerStatuses {
			if status.State.Running != nil {
				expected[status.Name] = true
			}
		}
		if len(expected) > MaxContainers {
			c.missing = "pod container bound exceeded"
			continue
		}
		found := map[string]bool{}
		for _, status := range statuses {
			// LastTerminationState belongs to the previous runtime ID. A restart
			// must never assign the new container's start/read time to that epoch.
			if term := status.LastTerminationState.Terminated; term != nil && term.ContainerID != "" {
				recordTermination(state, status.Name, term.ContainerID, term)
			}
			if term := status.State.Terminated; term != nil {
				id := term.ContainerID
				if id == "" {
					id = status.ContainerID
				}
				recordTermination(state, status.Name, id, term)
			}
			if !expected[status.Name] || status.ContainerID == "" {
				continue
			}
			found[status.Name] = true
			key := status.Name + "/" + status.ContainerID
			container := state.containers[key]
			if container == nil {
				if tracked >= MaxTrackedContainers {
					c.missing = "node tracked container bound exceeded"
					continue
				}
				tracked++
				container = &containerState{row: Container{Name: status.Name, ID: status.ContainerID}}
				state.containers[key] = container
			}
			if status.State.Running != nil {
				container.row.StartedAt = status.State.Running.StartedAt.Time
			}
			if status.State.Terminated != nil {
				term := status.State.Terminated
				if container.row.StartedAt.IsZero() {
					container.row.StartedAt = term.StartedAt.Time
				}
				recordTermination(state, status.Name, status.ContainerID, term)
			}
		}
		for name := range expected {
			if !found[name] {
				key := name + "/pending"
				if state.containers[key] == nil {
					if tracked >= MaxTrackedContainers {
						c.missing = "node tracked container bound exceeded"
						continue
					}
					tracked++
					state.containers[key] = &containerState{row: Container{Name: name, MissingReason: "container not running"}}
				}
			} else {
				delete(state.containers, name+"/pending")
			}
		}
	}
	cutoff := now.Add(-c.Retention)
	for uid, pod := range c.pods {
		if !seen[uid] {
			if pod.row.DeletedAt == nil {
				t := now
				pod.row.DeletedAt = &t
			}
		}
		if pod.lastSeen.Before(cutoff) {
			delete(c.pods, uid)
			continue
		}
		for key, container := range pod.containers {
			// API deletion or replacement does not stop execution. Continue
			// reading the known, UID-bound cgroup until it actually disappears.
			if container.row.ID != "" {
				c.sample(uid, container)
			}
			first := 0
			for first < len(container.row.Windows) && container.row.Windows[first].To.Before(cutoff) {
				first++
			}
			container.row.Windows = append([]Window(nil), container.row.Windows[first:]...)
			// Bound deleted/restarted container identities as well as samples.
			if len(container.row.Windows) == 0 && container.previous != nil && container.previous.At.Before(cutoff) {
				delete(pod.containers, key)
			}
			end := container.row.FinishedAt
			if end == nil {
				end = container.row.EndUpperBound
			}
			if len(container.row.Windows) == 0 && end != nil && end.Before(cutoff) {
				delete(pod.containers, key)
			}
		}
	}
	c.collectedAt = time.Now()
}

func recordTermination(pod *podState, name, id string, term *corev1.ContainerStateTerminated) {
	if id == "" || term == nil || term.FinishedAt.IsZero() {
		return
	}
	if term.ContainerID != "" && term.ContainerID != id {
		return
	}
	container := pod.containers[name+"/"+id]
	if container == nil {
		return
	}
	finished := term.FinishedAt.Time
	if !container.row.StartedAt.IsZero() && finished.Before(container.row.StartedAt) {
		return
	}
	container.row.FinishedAt = &finished
}

func (c *Collector) sample(uid string, container *containerState) {
	path := container.path
	if path == "" {
		var err error
		path, err = Resolve(c.Root, uid, container.row.ID)
		if err != nil {
			container.row.MissingReason = err.Error()
			container.previous = nil
			return
		}
	}
	counter, err := Read(path)
	if err != nil {
		container.row.MissingReason = "cgroup counters unavailable"
		container.previous = nil
		// Only absence of a previously resolved directory bounds termination.
		// A missing counter file/permission failure in an existing directory does
		// not prove that the process stopped, and cannot shorten accounting.
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) && container.path != "" && container.row.EndUpperBound == nil {
			observed := time.Now()
			container.row.EndUpperBound = &observed
		}
		return
	}
	container.row.EndUpperBound = nil
	if counter.At.Before(container.row.StartedAt) {
		container.row.MissingReason = "counter precedes container start"
		container.previous = nil
		return
	}
	container.row.MissingReason = ""
	if container.previous != nil && container.path == path {
		window, err := Difference(*container.previous, counter, 3*c.Interval)
		if err == nil {
			container.row.Windows = append(container.row.Windows, window)
			if len(container.row.Windows) > MaxWindows {
				container.row.Windows = append([]Window(nil), container.row.Windows[len(container.row.Windows)-MaxWindows:]...)
			}
		} else {
			container.row.MissingReason = err.Error()
		}
	} else {
		container.row.MissingReason = "awaiting second counter read"
	}
	container.previous = &counter
	container.path = path
}

func (c *Collector) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := Snapshot{Version: Protocol, Node: c.Node, CollectedAt: c.collectedAt, IntervalMillis: c.Interval.Milliseconds(), MissingReason: c.missing}
	for _, state := range c.pods {
		pod := state.row
		pod.Labels = map[string]string{}
		for key, value := range state.row.Labels {
			pod.Labels[key] = value
		}
		pod.Containers = nil
		for _, state := range state.containers {
			row := state.row
			row.Windows = append([]Window(nil), row.Windows...)
			pod.Containers = append(pod.Containers, row)
		}
		sort.Slice(pod.Containers, func(i, j int) bool {
			return pod.Containers[i].Name+pod.Containers[i].ID < pod.Containers[j].Name+pod.Containers[j].ID
		})
		s.Pods = append(s.Pods, pod)
	}
	sort.Slice(s.Pods, func(i, j int) bool { return s.Pods[i].UID < s.Pods[j].UID })
	return s
}

func (c *Collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/healthz" {
		c.mu.RLock()
		collectedAt, missing := c.collectedAt, c.missing
		c.mu.RUnlock()
		if collectedAt.IsZero() || time.Since(collectedAt) > 5*c.Interval || missing != "" {
			http.Error(w, "collector unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.URL.Path != "/snapshot" {
		http.NotFound(w, r)
		return
	}
	body, err := json.Marshal(c.Snapshot())
	if err != nil || len(body) > MaxSnapshotBytes {
		http.Error(w, "snapshot exceeds bound", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}
