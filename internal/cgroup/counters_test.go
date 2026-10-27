package cgroup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCounterDifferenceUnitsAndReset(t *testing.T) {
	at := time.Unix(1700000000, 0)
	a := uint64(100)
	b := uint64(600)
	window, err := Difference(Counters{At: at, Usage: 1e6, Throttled: &a}, Counters{At: at.Add(2 * time.Second), Usage: 2e6, Throttled: &b}, 6*time.Second)
	if err != nil || window.CPUSeconds != 1 || window.CPUSeconds/window.To.Sub(window.From).Seconds() != 0.5 || window.ThrottledSeconds == nil || *window.ThrottledSeconds != 0.0005 {
		t.Fatalf("bad units: %+v %v", window, err)
	}
	for _, later := range []Counters{{At: at, Usage: 2e6}, {At: at.Add(time.Second), Usage: 1}, {At: at.Add(7 * time.Second), Usage: 2e6}} {
		if _, err := Difference(Counters{At: at, Usage: 1e6}, later, 6*time.Second); err == nil {
			t.Fatalf("accepted reset/gap: %+v", later)
		}
	}
	window, err = Difference(Counters{At: at, Usage: 0}, Counters{At: at.Add(2 * time.Second), Usage: 0}, 6*time.Second)
	if err != nil || window.CPUSeconds != 0 {
		t.Fatal("zero must be retained")
	}
}

func TestResolveUIDRuntimeAndSymlinkIsolation(t *testing.T) {
	root := t.TempDir()
	uid := "11111111-2222-3333-4444-555555555555"
	id := strings.Repeat("a", 64)
	systemd := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod"+strings.ReplaceAll(uid, "-", "_")+".slice", "cri-containerd-"+id+".scope")
	if err := os.MkdirAll(systemd, 0755); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(root, uid, "containerd://"+id)
	if err != nil || resolved != systemd {
		t.Fatalf("resolve: %s %v", resolved, err)
	}
	for _, bad := range []struct{ uid, id string }{{"../escape", "containerd://" + id}, {uid, "containerd://../escape"}, {uid, "unknown://" + id}, {"recreated-uid", "containerd://" + id}} {
		if _, err := Resolve(root, bad.uid, bad.id); err == nil {
			t.Fatalf("accepted foreign/unsafe identity: %+v", bad)
		}
	}
	if err := os.Remove(systemd); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), systemd); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(root, uid, "containerd://"+id); err == nil {
		t.Fatal("accepted escaped symlink")
	}
	if err := os.Remove(systemd); err != nil {
		t.Fatal(err)
	}
	cgroupfs := filepath.Join(root, "kubepods", "burstable", "pod"+uid, id)
	if err := os.MkdirAll(cgroupfs, 0755); err != nil {
		t.Fatal(err)
	}
	if path, err := Resolve(root, uid, "containerd://"+id); err != nil || path != cgroupfs {
		t.Fatalf("cgroupfs unavailable: %s %v", path, err)
	}
}

func TestCounterParsingMissingAndOptionalMetrics(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cpu.stat"), []byte("usage_usec 2500000\nnr_periods 42\nnr_throttled 2\nthrottled_usec 100000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "memory.current"), []byte("1048576\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cpu.pressure"), []byte("some avg10=0.00 avg60=0.00 avg300=0.00 total=250\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=100\n"), 0644); err != nil {
		t.Fatal(err)
	}
	counter, err := Read(root)
	if err != nil || counter.Usage != 2500000 || counter.Memory == nil || *counter.Memory != 1048576 || counter.Some == nil || *counter.Some != 250 || counter.Full == nil || *counter.Full != 100 {
		t.Fatalf("bad counters %+v %v", counter, err)
	}
	if err := os.WriteFile(filepath.Join(root, "cpu.stat"), []byte("usage_usec 1\nusage_usec 2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(root); err == nil {
		t.Fatal("duplicate usage accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "cpu.stat"), []byte("usage_usec -1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(root); err == nil {
		t.Fatal("negative usage accepted")
	}
}
