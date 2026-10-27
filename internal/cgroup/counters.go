package cgroup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Counters struct {
	At                                               time.Time
	Usage                                            uint64
	Memory                                           *uint64
	MemoryAt                                         time.Time
	Throttled, Periods, ThrottledPeriods, Some, Full *uint64
}

var safeUID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,127}$`)
var safeContainer = regexp.MustCompile(`^[a-fA-F0-9]{12,128}$`)

// Resolve supports the documented Kubernetes systemd and cgroupfs layouts.
// Only API-derived pod UIDs and runtime container IDs enter candidate paths.
func Resolve(root, uid, containerID string) (string, error) {
	if !safeUID.MatchString(uid) {
		return "", fmt.Errorf("invalid pod UID")
	}
	parts := strings.Split(containerID, "://")
	if len(parts) != 2 || !safeContainer.MatchString(parts[1]) {
		return "", fmt.Errorf("invalid container ID")
	}
	prefix := ""
	switch parts[0] {
	case "containerd":
		prefix = "cri-containerd-"
	case "cri-o":
		prefix = "crio-"
	case "docker":
		prefix = "docker-"
	default:
		return "", fmt.Errorf("unsupported container runtime")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	var paths []string
	for _, qos := range []string{"", "burstable", "besteffort"} {
		base := "kubepods.slice"
		stem := "kubepods"
		if qos != "" {
			base = filepath.Join(base, "kubepods-"+qos+".slice")
			stem += "-" + qos
		}
		paths = append(paths, filepath.Join(base, stem+"-pod"+strings.ReplaceAll(uid, "-", "_")+".slice", prefix+parts[1]+".scope"))
		base = "kubepods"
		if qos != "" {
			base = filepath.Join(base, qos)
		}
		paths = append(paths, filepath.Join(base, "pod"+uid, parts[1]))
	}
	for _, rel := range paths {
		path := filepath.Join(root, rel)
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		within, err := filepath.Rel(root, resolved)
		if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(os.PathSeparator)) {
			continue
		}
		// Reject symlinks even within the mount: a valid container has an exact,
		// immutable directory, not an alias supplied by a different workload.
		if resolved != path {
			continue
		}
		if info, err := os.Stat(resolved); err == nil && info.IsDir() {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("cgroup-v2 container directory unavailable")
}

func keyValues(path string) (map[string]uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	values := map[string]uint64{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024), 16384)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid counter row")
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid counter value")
		}
		if _, found := values[fields[0]]; found {
			return nil, fmt.Errorf("duplicate counter")
		}
		values[fields[0]] = v
	}
	return values, scanner.Err()
}

func optional(values map[string]uint64, key string) *uint64 {
	if v, ok := values[key]; ok {
		return &v
	}
	return nil
}

func pressure(path string) (some, full *uint64) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		for _, field := range fields[1:] {
			if strings.HasPrefix(field, "total=") {
				if value, err := strconv.ParseUint(strings.TrimPrefix(field, "total="), 10, 64); err == nil {
					switch fields[0] {
					case "some":
						some = &value
					case "full":
						full = &value
					}
				}
			}
		}
	}
	return
}

func Read(path string) (Counters, error) {
	before := time.Now()
	values, err := keyValues(filepath.Join(path, "cpu.stat"))
	if err != nil {
		return Counters{}, err
	}
	usage, ok := values["usage_usec"]
	if !ok {
		return Counters{}, fmt.Errorf("cpu.stat has no usage_usec")
	}
	after := time.Now()
	c := Counters{At: before.Add(after.Sub(before) / 2), Usage: usage, Throttled: optional(values, "throttled_usec"), Periods: optional(values, "nr_periods"), ThrottledPeriods: optional(values, "nr_throttled")}
	memoryBefore := time.Now()
	if raw, err := os.ReadFile(filepath.Join(path, "memory.current")); err == nil {
		if value, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); err == nil {
			c.Memory = &value
		}
	}
	c.MemoryAt = memoryBefore.Add(time.Since(memoryBefore) / 2)
	c.Some, c.Full = pressure(filepath.Join(path, "cpu.pressure"))
	return c, nil
}

func delta(a, b *uint64, scale float64) *float64 {
	if a == nil || b == nil || *b < *a {
		return nil
	}
	v := float64(*b-*a) / scale
	return &v
}
func uintDelta(a, b *uint64) *uint64 {
	if a == nil || b == nil || *b < *a {
		return nil
	}
	v := *b - *a
	return &v
}

// Difference rejects resets and delayed reads instead of spreading a long gap
// into apparently fresh rates. A zero delta remains a genuine zero reading.
func Difference(a, b Counters, maxGap time.Duration) (Window, error) {
	if !b.At.After(a.At) || b.At.Sub(a.At) > maxGap || b.Usage < a.Usage {
		return Window{}, fmt.Errorf("CPU counter reset or collection gap")
	}
	return Window{From: a.At, To: b.At, CPUSeconds: float64(b.Usage-a.Usage) / 1e6, MemoryBytes: b.Memory, MemorySourceAt: b.MemoryAt, ThrottledSeconds: delta(a.Throttled, b.Throttled, 1e6), Periods: uintDelta(a.Periods, b.Periods), ThrottledPeriods: uintDelta(a.ThrottledPeriods, b.ThrottledPeriods), PressureSomeSeconds: delta(a.Some, b.Some, 1e6), PressureFullSeconds: delta(a.Full, b.Full, 1e6)}, nil
}
