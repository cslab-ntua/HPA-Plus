package metrics

import (
	"container/list"
	"context"
	"encoding/json"
	"sync"
	"time"

	gathered "github.com/jthomperoo/k8shorizmetrics/v2/metrics"
)

const (
	observationEntries    = 256
	observationBytes      = 16 << 20
	observationEntryBytes = 256 << 10
)

// Observations shares completed metric reads, never forecasts or replica
// decisions. Misses gather independently under the caller's own cancellation;
// a blocked or canceled reconciliation cannot hold up the sampler.
// Encoded snapshots give every consumer independent maps/slices/pointers, even
// when the evaluator mutates pod metrics for missing-pod substitutions.
type Observations struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List
	bytes   int
}

type observationEntry struct {
	owner, fingerprint string
	started, expires   time.Time
	data               []byte
}

func NewObservations() *Observations {
	return &Observations{entries: make(map[string]*list.Element), lru: list.New()}
}

func (s *Observations) remove(e *list.Element) {
	v := e.Value.(observationEntry)
	delete(s.entries, v.owner)
	s.bytes -= len(v.data)
	s.lru.Remove(e)
}

func (s *Observations) Forget(owner string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[owner]; e != nil {
		s.remove(e)
	}
}

// Read reuses only complete, timestamped observations while both the local
// collection deadline and optional source-age policy remain valid. TTL starts
// before gathering: slow I/O cannot make an old read appear newly collected.
func (s *Observations) Read(ctx context.Context, owner, fingerprint string, count int,
	ttl, maxAge time.Duration, now func() time.Time,
	gather func(context.Context) ([]*gathered.Metric, error),
) ([]*gathered.Metric, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || owner == "" || fingerprint == "" || ttl <= 0 {
		return gather(ctx)
	}
	started := now()
	s.mu.Lock()
	var cached []byte
	var collected, expires time.Time
	if e := s.entries[owner]; e != nil {
		v := e.Value.(observationEntry)
		if v.fingerprint == fingerprint && !started.Before(v.started) && started.Before(v.expires) {
			cached = v.data
			collected, expires = v.started, v.expires
			s.lru.MoveToFront(e)
		} else {
			s.remove(e)
		}
	}
	s.mu.Unlock()
	if cached != nil {
		var result []*gathered.Metric
		decodeErr := json.Unmarshal(cached, &result)
		checkedAt := now()
		if decodeErr == nil && !checkedAt.Before(collected) && checkedAt.Before(expires) && reusableObservations(result, count, checkedAt, maxAge) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return result, nil
		}
	}
	result, err := gather(ctx)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return result, err
	}
	completed := now()
	if completed.Before(started) || !completed.Before(started.Add(ttl)) || !reusableObservations(result, count, completed, maxAge) {
		return result, nil
	}
	data, err := json.Marshal(result)
	if err != nil || len(data) > observationEntryBytes {
		return result, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A late overlapping read must not replace a newer completed observation.
	if e := s.entries[owner]; e != nil {
		if e.Value.(observationEntry).started.After(started) {
			return result, nil
		}
		s.remove(e)
	}
	for s.lru.Len() >= observationEntries || s.bytes+len(data) > observationBytes {
		s.remove(s.lru.Back())
	}
	v := observationEntry{owner: owner, fingerprint: fingerprint, started: started, expires: started.Add(ttl), data: data}
	s.entries[owner] = s.lru.PushFront(v)
	s.bytes += len(data)
	return result, nil
}

func reusableObservations(values []*gathered.Metric, count int, now time.Time, maxAge time.Duration) bool {
	if count == 0 || len(values) != count {
		return false
	}
	for _, value := range values {
		times := observationTimestamps(value)
		if len(times) == 0 {
			return false
		}
		for _, stamp := range times {
			if stamp.IsZero() || stamp.After(now) || (maxAge > 0 && now.Sub(stamp) > maxAge) {
				return false
			}
		}
	}
	return true
}
