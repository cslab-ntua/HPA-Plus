package cgroup

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/labels"
)

func counterFixture() (time.Time, Snapshot) {
	at := time.Unix(1700000000, 0)
	memory := uint64(1024)
	s := Snapshot{Version: Protocol, Node: "n1", CollectedAt: at.Add(32 * time.Second), IntervalMillis: 2000}
	pod := Pod{Namespace: "a", Name: "deployment-a", UID: "uid-a", Labels: map[string]string{"app": "a"}, CreatedAt: at.Add(-time.Minute), Containers: []Container{{Name: "app", ID: "containerd://aaaa", StartedAt: at.Add(-time.Minute)}}}
	for i := 0; i < 16; i++ {
		pod.Containers[0].Windows = append(pod.Containers[0].Windows, Window{From: at.Add(time.Duration(2*i) * time.Second), To: at.Add(time.Duration(2*i+2) * time.Second), CPUSeconds: 1, MemoryBytes: &memory})
	}
	s.Pods = []Pod{pod}
	return at, s
}

func TestIntegrateKnownWindowUnitsAndBoundaries(t *testing.T) {
	at, s := counterFixture()
	r, err := Integrate([]Snapshot{s}, "a", labels.SelectorFromSet(labels.Set{"app": "a"}), at.Add(time.Second), at.Add(31*time.Second), at.Add(33*time.Second))
	if err != nil || !r.Complete || r.Coverage != 1 || r.CPUSeconds != 15 || r.RunningPodSeconds != 30 || r.MemoryBytes == nil || *r.MemoryBytes != 1024 || !r.BoundaryAveraged {
		t.Fatalf("bad integration %+v %v", r, err)
	}
	if !r.SourceAt.Equal(at.Add(32 * time.Second)) {
		t.Fatal("boundary observation availability was backdated")
	}
}

func TestMissingAndOverlappingWindows(t *testing.T) {
	at, s := counterFixture()
	s.Pods[0].Containers[0].Windows = append(s.Pods[0].Containers[0].Windows[:4], s.Pods[0].Containers[0].Windows[5:]...)
	r, err := Integrate([]Snapshot{s}, "a", labels.Everything(), at, at.Add(30*time.Second), at.Add(33*time.Second))
	if err != nil || r.Complete || math.Abs(r.Coverage-28.0/30) > 1e-9 || r.CPUSeconds != 14 || len(r.MissingReasons) == 0 {
		t.Fatalf("gap replaced or hidden: %+v %v", r, err)
	}
	s.Pods[0].Containers[0].Windows = append(s.Pods[0].Containers[0].Windows, s.Pods[0].Containers[0].Windows[0])
	if _, err := Integrate([]Snapshot{s}, "a", labels.Everything(), at, at.Add(30*time.Second), at.Add(33*time.Second)); err == nil {
		t.Fatal("duplicate counter windows accepted")
	}
}

func TestPodNamespaceUIDAndLifetimeIsolation(t *testing.T) {
	at, s := counterFixture()
	foreign := s.Pods[0]
	foreign.Namespace = "b"
	foreign.UID = "uid-b"
	s.Pods = append(s.Pods, foreign)
	r, err := Integrate([]Snapshot{s}, "a", labels.Everything(), at, at.Add(30*time.Second), at.Add(33*time.Second))
	if err != nil || r.PodCount != 1 || r.CPUSeconds != 15 {
		t.Fatalf("namespace leakage %+v %v", r, err)
	}
	s.Pods = append(s.Pods, s.Pods[0])
	if _, err := Integrate([]Snapshot{s}, "a", labels.Everything(), at, at.Add(30*time.Second), at.Add(33*time.Second)); err == nil {
		t.Fatal("UID duplicated across sources")
	}
	at, s = counterFixture()
	finished := at.Add(10 * time.Second)
	s.Pods[0].Containers[0].FinishedAt = &finished
	r, err = Integrate([]Snapshot{s}, "a", labels.Everything(), at, at.Add(30*time.Second), at.Add(33*time.Second))
	if err != nil || r.CPUSeconds != 5 || r.RunningPodSeconds != 10 || !r.Complete {
		t.Fatalf("terminated container extrapolated %+v %v", r, err)
	}
}

func TestMemoryDoesNotUseFutureReadingAndStaleIsRejected(t *testing.T) {
	at, s := counterFixture()
	old := uint64(512)
	future := uint64(999999)
	for i := range s.Pods[0].Containers[0].Windows {
		window := &s.Pods[0].Containers[0].Windows[i]
		window.MemoryBytes = &old
		if window.To.After(at.Add(29 * time.Second)) {
			window.MemoryBytes = &future
		}
	}
	r, err := Integrate([]Snapshot{s}, "a", labels.Everything(), at, at.Add(29*time.Second), at.Add(33*time.Second))
	if err != nil || r.MemoryBytes == nil || *r.MemoryBytes != 512 {
		t.Fatalf("future memory leak: %+v %v", r, err)
	}
	if _, err := Integrate([]Snapshot{s}, "a", labels.Everything(), at, at.Add(30*time.Second), at.Add(60*time.Second)); err == nil {
		t.Fatal("stale envelope accepted")
	}
}

func TestAnyUnmeasuredTerminationTailRemainsIncomplete(t *testing.T) {
	at, s := counterFixture()
	tailEnd := at.Add(30*time.Second + time.Nanosecond)
	s.Pods[0].Containers[0].Windows = s.Pods[0].Containers[0].Windows[:15]
	s.Pods[0].Containers[0].EndUpperBound = &tailEnd
	r, err := Integrate([]Snapshot{s}, "a", labels.Everything(), at, tailEnd, at.Add(33*time.Second))
	if err != nil || r.Complete || r.Coverage >= 1 {
		t.Fatalf("submillisecond unknown tail was erased %+v %v", r, err)
	}
}

func TestMemoryPointExcludesFinishedEpochAndRetainsActualSourceTime(t *testing.T) {
	at, s := counterFixture()
	huge := uint64(999999)
	finished := at.Add(10 * time.Second)
	old := Container{Name: "app", ID: "old", StartedAt: at, FinishedAt: &finished, Windows: []Window{{From: at, To: finished, CPUSeconds: 1, MemoryBytes: &huge}}}
	s.Pods[0].Containers = append(s.Pods[0].Containers, old)
	r, err := Integrate([]Snapshot{s}, "a", labels.Everything(), at, at.Add(29*time.Second), at.Add(33*time.Second))
	if err != nil || r.MemoryBytes == nil || *r.MemoryBytes != 1024 || !r.MemorySourceAt.Equal(at.Add(28*time.Second)) {
		t.Fatalf("old epoch/future CPU timestamp contaminated memory point %+v %v", r, err)
	}
	// The memory read happens after the CPU counter read. A reading actually
	// taken after queryTo cannot be backdated to the CPU timestamp.
	last := &s.Pods[0].Containers[0].Windows[13]
	last.MemorySourceAt = at.Add(30 * time.Second)
	last.MemoryBytes = &huge
	r, err = Integrate([]Snapshot{s}, "a", labels.Everything(), at, at.Add(29*time.Second), at.Add(33*time.Second))
	if err != nil || r.MemoryBytes == nil || *r.MemoryBytes != 1024 || !r.MemorySourceAt.Equal(at.Add(26*time.Second)) {
		t.Fatalf("future memory source accepted %+v %v", r, err)
	}
}

func TestMemoryPointUnavailableForUnknownEndOrMissingNewestValue(t *testing.T) {
	at, s := counterFixture()
	upper := at.Add(32 * time.Second)
	s.Pods[0].Containers[0].EndUpperBound = &upper
	s.Pods[0].Containers[0].Windows = s.Pods[0].Containers[0].Windows[:14]
	r, err := Integrate([]Snapshot{s}, "a", labels.Everything(), at, at.Add(30*time.Second), at.Add(33*time.Second))
	if err != nil || r.MemoryBytes != nil {
		t.Fatalf("unknown running endpoint supplied memory %+v %v", r, err)
	}
	at, s = counterFixture()
	s.Pods[0].Containers[0].Windows[14].MemoryBytes = nil
	r, err = Integrate([]Snapshot{s}, "a", labels.Everything(), at, at.Add(30*time.Second), at.Add(33*time.Second))
	if err != nil || r.MemoryBytes != nil {
		t.Fatalf("missing newest memory revived older value %+v %v", r, err)
	}
}

func TestRunningPodSecondsUsesLifetimeUnionAcrossRestartGap(t *testing.T) {
	at, s := counterFixture()
	firstEnd := at.Add(10 * time.Second)
	old := Container{Name: "app", ID: "containerd://old", StartedAt: at, FinishedAt: &firstEnd, Windows: []Window{{From: at, To: firstEnd, CPUSeconds: 5}}}
	fresh := Container{Name: "app", ID: "containerd://new", StartedAt: at.Add(20 * time.Second), Windows: []Window{{From: at.Add(20 * time.Second), To: at.Add(30 * time.Second), CPUSeconds: 5}}}
	s.Pods[0].Containers = []Container{old, fresh}
	r, err := Integrate([]Snapshot{s}, "a", labels.Everything(), at, at.Add(30*time.Second), at.Add(33*time.Second))
	if err != nil || !r.Complete || r.RunningPodSeconds != 20 || r.CPUSeconds != 10 || r.Coverage != 1 {
		t.Fatalf("restart gap counted as running %+v %v", r, err)
	}
	// A sidecar alive across the gap keeps the pod running, counted once.
	sidecar := Container{Name: "sidecar", ID: "containerd://sidecar", StartedAt: at}
	for i := 0; i < 15; i++ {
		sidecar.Windows = append(sidecar.Windows, Window{From: at.Add(time.Duration(2*i) * time.Second), To: at.Add(time.Duration(2*i+2) * time.Second), CPUSeconds: 0})
	}
	s.Pods[0].Containers = append(s.Pods[0].Containers, sidecar)
	r, err = Integrate([]Snapshot{s}, "a", labels.Everything(), at, at.Add(30*time.Second), at.Add(33*time.Second))
	if err != nil || !r.Complete || r.RunningPodSeconds != 30 {
		t.Fatalf("sidecar lifetime duplicated/omitted %+v %v", r, err)
	}
}

func TestInventoryRetainsFinishedOutsideWindowWithoutCountingCapacity(t *testing.T) {
	at, snapshot := counterFixture()
	finished := at.Add(-time.Second)
	old := Pod{Namespace: "a", Name: "replaced-pod", UID: "old-uid", Labels: map[string]string{"app": "a"},
		Containers: []Container{{Name: "app", ID: "old-container", StartedAt: at.Add(-time.Minute), FinishedAt: &finished}}}
	snapshot.Pods = append(snapshot.Pods, old)
	foreign := old
	foreign.Namespace, foreign.UID = "b", "foreign-uid"
	snapshot.Pods = append(snapshot.Pods, foreign)
	result, err := Integrate([]Snapshot{snapshot}, "a", labels.SelectorFromSet(labels.Set{"app": "a"}), at, at.Add(30*time.Second), at.Add(33*time.Second))
	if err != nil || !result.Complete || result.Coverage != 1 || result.PodCount != 1 || len(result.Pods) != 1 || result.CPUSeconds != 15 || result.RunningPodSeconds != 30 {
		t.Fatalf("historical inventory changed active accounting: %+v %v", result, err)
	}
	if len(result.Inventory) != 2 || result.Inventory[1].UID != "old-uid" || result.Inventory[1].Containers[0].FinishedAt == nil || !result.Inventory[1].Containers[0].FinishedAt.Equal(finished) {
		t.Fatalf("exact historical lifecycle was lost from scoped inventory: %+v", result.Inventory)
	}
}

func TestNativeBatchAndLifecycleCompletenessRemainIndependentOfCounterGaps(t *testing.T) {
	at, snapshot := counterFixture()
	snapshot.Pods[0].Containers[0].Windows = append(snapshot.Pods[0].Containers[0].Windows[:4], snapshot.Pods[0].Containers[0].Windows[5:]...)
	input := []Snapshot{snapshot}
	before, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Integrate(input, "a", labels.SelectorFromSet(labels.Set{"app": "a"}), at, at.Add(30*time.Second), at.Add(33*time.Second))
	if err != nil || result.Complete || !result.InventoryComplete || len(result.NativeSnapshots) != 1 {
		t.Fatalf("counter support and runtime inventory conflated: %+v %v", result, err)
	}
	after, err := json.Marshal(snapshot)
	if err != nil || string(before) != string(after) {
		t.Fatalf("integration mutated shared node batch: before=%s after=%s err=%v", before, after, err)
	}
	// A caller can replace its own top-level filtered snapshot slice without
	// modifying the input envelope. Nested native data remain explicitly read-only.
	result.NativeSnapshots[0].Pods = nil
	if len(input[0].Pods) != 1 {
		t.Fatal("native envelope slice aliases the caller's top-level snapshot")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]interface{}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if _, exists := wire["NativeSnapshots"]; exists {
		t.Fatal("native batch leaked into status JSON")
	}
	if _, exists := wire["nativeSnapshots"]; exists {
		t.Fatal("native batch leaked into status JSON")
	}

	at, snapshot = counterFixture()
	snapshot.Pods[0].Containers = append(snapshot.Pods[0].Containers, Container{Name: "unknown"})
	result, err = Integrate([]Snapshot{snapshot}, "a", labels.Everything(), at, at.Add(30*time.Second), at.Add(33*time.Second))
	if err != nil || result.Complete || result.InventoryComplete {
		t.Fatalf("unknown runtime claimed trusted population inventory: %+v %v", result, err)
	}
	snapshot.Pods[0].Containers = nil
	result, err = Integrate([]Snapshot{snapshot}, "a", labels.Everything(), at, at.Add(30*time.Second), at.Add(33*time.Second))
	if err != nil || result.InventoryComplete {
		t.Fatalf("missing runtime population claimed complete inventory: %+v %v", result, err)
	}
}

func TestFutureCreatedUnknownPodRetainsInventoryWithoutInvalidatingPastCPU(t *testing.T) {
	at, snapshot := counterFixture()
	future := Pod{Namespace: "a", Name: "future", UID: "future-uid", CreatedAt: at.Add(31 * time.Second),
		Containers: []Container{{Name: "not-started"}}}
	snapshot.Pods = append(snapshot.Pods, future)
	result, err := Integrate([]Snapshot{snapshot}, "a", labels.Everything(), at, at.Add(30*time.Second), at.Add(33*time.Second))
	if err != nil || !result.Complete || !result.InventoryComplete || result.CPUSeconds != 15 || result.RunningPodSeconds != 30 || len(result.Inventory) != 2 || result.PodCount != 1 {
		t.Fatalf("future creation contaminated closed-window accounting: %+v %v", result, err)
	}
}
