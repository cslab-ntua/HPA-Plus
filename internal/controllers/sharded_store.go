package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
	"github.com/cslab-ntua/HPA-Plus/internal/history"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	maxAllocatedHistoryBytes = history.MaxAllocatedBytes
	historyChunkBytes        = history.ChunkBytes
	historyRootLabel         = history.RootLabel
	historyFenceKey          = history.FenceKey
)

type historyChunk = history.ChunkReference
type historyManifest = history.Manifest
type allocatedRevision struct {
	root        *corev1.ConfigMap
	manifest    *historyManifest
	stateDigest [32]byte
}

// AllocatingStore automatically spills state into immutable ConfigMap chunks.
// One root resourceVersion CAS publishes the complete state and journal. A
// monotonic root fence makes stale writers unable to publish chunks reclaimed
// by a newer writer; cleanup never relies on clocks, leases or resourceVersion order.
type AllocatingStore struct{ ConfigMapStore }

func historyRootKey(owner *api.HPAPlus) client.ObjectKey {
	return client.ObjectKey{Namespace: owner.Namespace, Name: "hpa-plus-" + owner.Name + "-data"}
}
func ownedHistory(cm *corev1.ConfigMap, owner *api.HPAPlus) bool {
	return history.OwnedConfigMap(cm, owner)
}
func chunkPrefix(root *corev1.ConfigMap) string    { return history.ChunkPrefix(root) }
func fenceOf(cm *corev1.ConfigMap) (uint64, error) { return history.FenceOf(cm) }

func (s *AllocatingStore) Load(ctx context.Context, owner *api.HPAPlus) (result *history.Snapshot, readErr error) {
	started := time.Now()
	defer func() {
		readErr = storageFailure("HistoryReadFailed", readErr)
		s.Telemetry.ObserveHistoryOperation("read", historyOperationOutcome(ctx, readErr, "success"), time.Since(started))
		if result != nil {
			s.Telemetry.ObserveHistoryBytes("read", result.StateBytes+result.JournalBytes)
		}
	}()
	for attempt := 0; attempt < 5; attempt++ {
		snapshot, root, err := s.loadOnce(ctx, owner)
		if err == nil {
			return snapshot, nil
		}
		if root != nil {
			latest := &corev1.ConfigMap{}
			if getErr := s.Reader.Get(ctx, historyRootKey(owner), latest); getErr == nil && (latest.UID != root.UID || latest.ResourceVersion != root.ResourceVersion) {
				continue
			}
		}
		return nil, storageFailure("HistoryDataInvalid", err)
	}
	return nil, fmt.Errorf("history changed repeatedly while reading")
}
func (s *AllocatingStore) loadOnce(ctx context.Context, owner *api.HPAPlus) (*history.Snapshot, *corev1.ConfigMap, error) {
	root := &corev1.ConfigMap{}
	if err := s.Reader.Get(ctx, historyRootKey(owner), root); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, nil, storageFailure("HistoryReadFailed", err)
		}
		legacy := s.ConfigMapStore
		legacy.Telemetry = nil
		snapshot, err := legacy.Load(ctx, owner)
		if err == nil {
			snapshot.StateBudgetBytes = maxAllocatedHistoryBytes
			snapshot.Revision = &allocatedRevision{root: snapshot.Revision.(*corev1.ConfigMap)}
		}
		return snapshot, nil, err
	}
	if !ownedHistory(root, owner) {
		return nil, root, storageFailure("HistoryOwnershipMismatch", fmt.Errorf("history root is owned by another resource"))
	}
	if configMapDataBytes(root) > maxConfigMapDataBytes {
		return nil, root, fmt.Errorf("history root exceeds ConfigMap limit")
	}
	var header struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal([]byte(root.Data[configMapDataKey]), &header); err != nil && root.Data[configMapDataKey] != "" {
		return nil, root, err
	}
	if header.SchemaVersion != 2 {
		var data *api.HPAPlusData
		var err error
		if root.Data[configMapDataKey] == "" {
			data = &api.HPAPlusData{ModelHistories: map[string]api.ModelHistory{}}
		} else {
			data, err = decodeHistory(root.Data[configMapDataKey])
		}
		if err != nil {
			return nil, root, err
		}
		snapshot, err := configMapSnapshot(root, data)
		if err == nil {
			snapshot.StateBudgetBytes = maxAllocatedHistoryBytes
			snapshot.Revision = &allocatedRevision{root: root}
		}
		return snapshot, root, err
	}
	manifest, err := decodeManifest(root.Data[configMapDataKey], root)
	if err != nil {
		return nil, root, err
	}
	state := make([]byte, 0)
	for _, ref := range manifest.Chunks {
		chunk := &corev1.ConfigMap{}
		if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: root.Namespace, Name: ref.Name}, chunk); err != nil {
			return nil, root, err
		}
		bytes := chunk.BinaryData["state"]
		digest := sha256.Sum256(bytes)
		if !ownedHistory(chunk, owner) || chunk.Labels[historyRootLabel] != string(root.UID) || string(chunk.UID) != ref.UID || chunk.Immutable == nil || !*chunk.Immutable || len(bytes) != ref.Bytes || hex.EncodeToString(digest[:]) != ref.Digest {
			return nil, root, fmt.Errorf("history chunk identity/content mismatch")
		}
		state = append(state, bytes...)
	}
	data, err := history.DecodeBounded(string(state), maxAllocatedHistoryBytes)
	if err != nil {
		return nil, root, err
	}
	if data.PendingScale != nil {
		return nil, root, fmt.Errorf("state chunks contain a competing scale journal")
	}
	data.PendingScale = manifest.PendingScale
	extra := configMapDataBytes(root) - len(root.Data[configMapDataKey])
	journal, err := journalSize(data)
	if err != nil {
		return nil, root, err
	}
	return &history.Snapshot{Data: data, Revision: &allocatedRevision{root: root, manifest: manifest, stateDigest: sha256.Sum256(state)}, UnrelatedBytes: extra, StateBytes: len(state) + extra, JournalBytes: journal, StateBudgetBytes: maxAllocatedHistoryBytes, JournalBudgetBytes: maxScaleJournalBytes}, root, nil
}

func decodeManifest(payload string, root *corev1.ConfigMap) (*historyManifest, error) {
	return history.DecodeManifest(payload, root)
}
func journalSize(data *api.HPAPlusData) (int, error) {
	if data.PendingScale == nil {
		return 0, nil
	}
	raw, err := json.Marshal(data.PendingScale)
	return len(raw) + len(`,"pendingScale":`), err
}

func (s *AllocatingStore) Commit(ctx context.Context, snapshot *history.Snapshot, data *api.HPAPlusData) (writeErr error) {
	started := time.Now()
	defer func() {
		writeErr = storageFailure("HistoryWriteFailed", writeErr)
		s.Telemetry.ObserveHistoryOperation("write", historyOperationOutcome(ctx, writeErr, "success"), time.Since(started))
	}()
	rev, ok := snapshot.Revision.(*allocatedRevision)
	if !ok || rev.root == nil {
		return fmt.Errorf("invalid allocated history revision")
	}
	state := *data
	state.PendingScale = nil
	encoded, err := encodeHistory(&state)
	if err != nil {
		return storageFailure("HistoryDataInvalid", err)
	}
	journal, err := journalSize(data)
	if err != nil {
		return storageFailure("HistoryDataInvalid", err)
	}
	extra := configMapDataBytes(rev.root) - len(rev.root.Data[configMapDataKey])
	s.Telemetry.ObserveHistoryBytes("write", len(encoded)+extra+journal)
	if len(encoded)+extra > maxAllocatedHistoryBytes || (journal > maxScaleJournalBytes && journal > snapshot.JournalBytes) {
		return storageFailure("HistoryCapacityExceeded", fmt.Errorf("allocated history exceeds state or journal budget; no samples were evicted"))
	}
	if rev.manifest == nil && len(encoded)+extra <= maxHistoryStateBytes && journal <= maxScaleJournalBytes {
		copy := *snapshot
		copy.Revision = rev.root
		legacy := s.ConfigMapStore
		legacy.Telemetry = nil
		return legacy.Commit(ctx, &copy, data)
	}
	if rev.root.UID == "" {
		return fmt.Errorf("history allocation requires a root UID")
	}
	fence, err := fenceOf(rev.root)
	if err != nil {
		return err
	}
	fence++
	root := rev.root.DeepCopy()
	if root.Annotations == nil {
		root.Annotations = map[string]string{}
	}
	root.Annotations[historyFenceKey] = strconv.FormatUint(fence, 10)
	// Advancing the fence invalidates all prior writers before garbage collection.
	// Readers still see the previous complete snapshot until the final root CAS.
	if err := s.Client.Update(ctx, root); err != nil {
		return err
	}
	if err := s.cleanup(ctx, root, rev.manifest, fence); err != nil {
		return err
	}
	manifest := &historyManifest{SchemaVersion: 2, PendingScale: data.PendingScale}
	// Journal-only updates reuse current immutable chunks when state is unchanged.
	if rev.manifest != nil && rev.stateDigest == sha256.Sum256(encoded) {
		manifest.Chunks = rev.manifest.Chunks
	} else {
		transaction := strings.ReplaceAll(uuid.New().String(), "-", "")
		for start := 0; start < len(encoded); start += historyChunkBytes {
			end := start + historyChunkBytes
			if end > len(encoded) {
				end = len(encoded)
			}
			bytes := encoded[start:end]
			digest := sha256.Sum256(bytes)
			immutable := true
			chunk := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s%s-%02d", chunkPrefix(root), transaction, len(manifest.Chunks)), Namespace: root.Namespace,
				OwnerReferences: root.OwnerReferences, Labels: map[string]string{historyRootLabel: string(root.UID)}, Annotations: map[string]string{historyFenceKey: strconv.FormatUint(fence, 10)}}, Immutable: &immutable, BinaryData: map[string][]byte{"state": bytes}}
			if err := s.Client.Create(ctx, chunk); err != nil {
				return storageFailure("HistoryWriteFailed", err)
			}
			if chunk.UID == "" {
				return fmt.Errorf("created history chunk has no UID")
			}
			manifest.Chunks = append(manifest.Chunks, historyChunk{Name: chunk.Name, UID: string(chunk.UID), Digest: hex.EncodeToString(digest[:]), Bytes: len(bytes)})
		}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	root.Data[configMapDataKey] = string(raw)
	if configMapDataBytes(root) > maxConfigMapDataBytes {
		return storageFailure("HistoryCapacityExceeded", fmt.Errorf("history root metadata and journal exceed ConfigMap limit"))
	}
	if snapshot.Validate != nil {
		if err := snapshot.Validate(); err != nil {
			return err
		}
	}
	if err := s.Client.Update(ctx, root); err != nil {
		return err
	}
	// Publication succeeded. Cleanup is retried by a later write if it fails.
	_ = s.cleanup(ctx, root, manifest, fence)
	return nil
}

func (s *AllocatingStore) cleanup(ctx context.Context, root *corev1.ConfigMap, manifest *historyManifest, fence uint64) error {
	keep := map[string]bool{}
	if manifest != nil {
		for _, ref := range manifest.Chunks {
			keep[ref.Name] = true
		}
	}
	var continuation string
	for {
		list := &corev1.ConfigMapList{}
		if err := s.Reader.List(ctx, list, client.InNamespace(root.Namespace), client.MatchingLabels{historyRootLabel: string(root.UID)}, client.Limit(64), client.Continue(continuation)); err != nil {
			return err
		}
		for i := range list.Items {
			chunk := &list.Items[i]
			generation, err := fenceOf(chunk)
			if err != nil || generation >= fence || keep[chunk.Name] || !strings.HasPrefix(chunk.Name, chunkPrefix(root)) || !sameOwners(root, chunk) {
				continue
			}
			uid, rv := chunk.UID, chunk.ResourceVersion
			if err := s.Client.Delete(ctx, chunk, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
		continuation = list.Continue
		if continuation == "" {
			return nil
		}
	}
}
func sameOwners(root, chunk *corev1.ConfigMap) bool {
	for _, a := range root.OwnerReferences {
		for _, b := range chunk.OwnerReferences {
			if a.Kind == "HPAPlus" && a.APIVersion == api.GroupVersion.String() && a.UID != "" && a.UID == b.UID && a.Name == b.Name && a.Kind == b.Kind && a.APIVersion == b.APIVersion {
				return true
			}
		}
	}
	return false
}

var _ history.Store = (*AllocatingStore)(nil)
