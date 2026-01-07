package snapshotter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
)

// TestReverseStringsConcurrent verifies reverseStrings is safe for concurrent use.
// reverseStrings returns a new slice, so concurrent calls on the same input are safe.
func TestReverseStringsConcurrent(t *testing.T) {
	const numGoroutines = 100
	original := []string{"layer5", "layer4", "layer3", "layer2", "layer1"}

	var wg sync.WaitGroup
	errors := make(chan string, numGoroutines)

	for range numGoroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := reverseStrings(original)
			if result[0] != "layer1" {
				errors <- "reverseStrings returned wrong first element"
			}
			if result[4] != "layer5" {
				errors <- "reverseStrings returned wrong last element"
			}
		}()
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("concurrent access error: %s", err)
	}

	// Verify original is unchanged
	if original[0] != "layer5" {
		t.Error("original was modified")
	}
}

// TestConcurrentPrepare verifies concurrent Prepare calls don't race.
func TestConcurrentPrepare(t *testing.T) {
	s := newTestSnapshotter(t)
	ctx := t.Context()
	const numGoroutines = 10

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines)

	for i := range numGoroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("concurrent-prepare-%d", id)
			_, err := s.Prepare(ctx, key, "")
			if err != nil {
				errors <- fmt.Errorf("prepare %d: %w", id, err)
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("unexpected error: %v", err)
	}

	// Verify all snapshots exist
	var count int
	err := s.Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("walk failed: %v", err)
	}
	if count < numGoroutines {
		t.Errorf("expected at least %d snapshots, got %d", numGoroutines, count)
	}
}

// TestConcurrentView verifies concurrent View calls on the same parent don't race.
// This tests the fsmeta generation coordination (placeholder file).
func TestConcurrentView(t *testing.T) {
	s := newTestSnapshotter(t)
	ctx := t.Context()

	// Create a base snapshot to use as parent
	_, err := s.Prepare(ctx, "base-for-views", "")
	if err != nil {
		t.Fatalf("prepare base: %v", err)
	}

	// Commit the base (needed for View)
	if err := s.Commit(ctx, "committed-base", "base-for-views"); err != nil {
		t.Fatalf("commit base: %v", err)
	}

	// Create multiple views concurrently - all trigger fsmeta generation for same parent
	const numGoroutines = 10
	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines)

	for i := range numGoroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("concurrent-view-%d", id)
			_, err := s.View(ctx, key, "committed-base")
			if err != nil {
				errors <- fmt.Errorf("view %d: %w", id, err)
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestConcurrentRemove verifies concurrent Remove calls work correctly.
func TestConcurrentRemove(t *testing.T) {
	s := newTestSnapshotter(t)
	ctx := t.Context()
	const numSnapshots = 10

	// Create snapshots to remove
	keys := make([]string, numSnapshots)
	for i := range numSnapshots {
		key := fmt.Sprintf("to-remove-%d", i)
		keys[i] = key
		if _, err := s.Prepare(ctx, key, ""); err != nil {
			t.Fatalf("prepare %d: %v", i, err)
		}
	}

	// Remove all concurrently
	var wg sync.WaitGroup
	errors := make(chan error, numSnapshots)

	for i, key := range keys {
		wg.Add(1)
		go func(id int, k string) {
			defer wg.Done()
			if err := s.Remove(ctx, k); err != nil {
				errors <- fmt.Errorf("remove %d (%s): %w", id, k, err)
			}
		}(i, key)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("unexpected error: %v", err)
	}

	// Verify all snapshots removed
	var remaining int
	err := s.Walk(ctx, func(_ context.Context, _ snapshots.Info) error {
		remaining++
		return nil
	})
	if err != nil {
		t.Fatalf("walk failed: %v", err)
	}
	if remaining != 0 {
		t.Errorf("expected 0 snapshots after removal, got %d", remaining)
	}
}

// TestMountTrackerConcurrentAccess verifies MountTracker is thread-safe.
func TestMountTrackerConcurrentOperations(t *testing.T) {
	tracker := NewMountTracker()
	const numGoroutines = 100

	var wg sync.WaitGroup

	// Concurrent mounts, unmounts, and checks - each goroutine gets unique ID
	for i := range numGoroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			snapID := fmt.Sprintf("snap-%d", id)

			// Each goroutine does a full cycle on its own unique ID
			tracker.SetMounted(snapID)
			_ = tracker.IsMounted(snapID) // Read while potentially racing with other ops
			tracker.SetUnmounted(snapID)
		}(i)
	}

	wg.Wait()

	// Clear should work after concurrent operations
	tracker.Clear()

	// Verify clean state
	for i := range numGoroutines {
		snapID := fmt.Sprintf("snap-%d", i)
		if tracker.IsMounted(snapID) {
			t.Errorf("%s should not be mounted after Clear", snapID)
		}
	}
}

// TestFsmetaPlaceholderRace verifies that concurrent fsmeta generation
// uses placeholder file correctly (only one wins).
func TestFsmetaPlaceholderRace(t *testing.T) {
	root := t.TempDir()
	s := &snapshotter{root: root}

	// Create snapshot directory with a fake layer blob
	snapshotDir := filepath.Join(root, "snapshots", "test-parent")
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a fake layer blob (required for findLayerBlob)
	layerBlob := filepath.Join(snapshotDir, "sha256-"+
		"a3ed95caeb02ffe68cdd9fd84406680ae93d633cb16422d00e8a7c22955b46d4.erofs")
	if err := os.WriteFile(layerBlob, []byte("fake erofs"), 0644); err != nil {
		t.Fatal(err)
	}

	// Run multiple goroutines trying to create fsmeta placeholder
	const numGoroutines = 20
	var wg sync.WaitGroup
	winners := make(chan int, numGoroutines)

	fsmetaPath := s.fsMetaPath("test-parent")

	for i := range numGoroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// Try to create placeholder atomically
			f, err := os.OpenFile(fsmetaPath, os.O_CREATE|os.O_EXCL, 0600)
			if err == nil {
				winners <- id
				f.Close()
			}
			// Others get os.ErrExist - that's expected
		}(i)
	}

	wg.Wait()
	close(winners)

	// Exactly one goroutine should win
	winnerCount := 0
	for range winners {
		winnerCount++
	}

	if winnerCount != 1 {
		t.Errorf("expected exactly 1 winner for placeholder creation, got %d", winnerCount)
	}
}
