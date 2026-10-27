package history

import (
	"context"
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

// Snapshot is a consistent, caller-owned logical history plus backend accounting.
// Revision is opaque to callers and must be returned to the same Store on Commit.
// Implementations reject stale revisions with a Kubernetes conflict error so the
// orchestrator can reread, revalidate identity and rerun its mutation.
type Snapshot struct {
	// Validate is the caller's final identity/freshness check. Stores invoke it
	// immediately before publishing, after any staging I/O. It may be nil.
	Validate                                 func() error
	Data                                     *api.HPAPlusData
	Revision                                 interface{}
	UnrelatedBytes, StateBytes, JournalBytes int
	StateBudgetBytes, JournalBudgetBytes     int
}

// Store commits complete snapshots atomically from the caller's perspective.
// Load validates ownership and wire data. Commit preserves unrelated backend data,
// enforces byte limits, and never evicts training rows to make a write fit.
type Store interface {
	Load(context.Context, *api.HPAPlus) (*Snapshot, error)
	Commit(context.Context, *Snapshot, *api.HPAPlusData) error
}
