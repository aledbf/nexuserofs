package snapshotter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
)

// TestParallelUnpackParentNotReady verifies that when a child layer's Prepare
// is called before the parent is committed, we get an appropriate error that
// containerd can handle via retry.
func TestParallelUnpackParentNotReady(t *testing.T) {
	s := newTestSnapshotter(t)
	ctx := t.Context()

	t.Run("parent exists but not committed", func(t *testing.T) {
		// Create a base snapshot but don't commit it yet
		_, err := s.Prepare(ctx, "base-layer", "")
		if err != nil {
			t.Fatalf("prepare base: %v", err)
		}

		// Try to prepare a child that references the uncommitted base
		// Containerd returns InvalidArgument when parent exists but isn't committed
		_, err = s.Prepare(ctx, "child-layer", "base-layer")
		if err == nil {
			t.Fatal("expected error when parent is not committed")
		}

		// The error should be InvalidArgument (parent exists but not committed)
		if !errdefs.IsInvalidArgument(err) {
			t.Errorf("expected InvalidArgument error, got: %v", err)
		}
	})

	t.Run("parent does not exist", func(t *testing.T) {
		// Try to prepare a child that references a non-existent parent
		// Containerd returns NotFound when parent doesn't exist
		_, err := s.Prepare(ctx, "orphan-layer", "non-existent-parent")
		if err == nil {
			t.Fatal("expected error when parent does not exist")
		}

		// The error should be NotFound
		if !errdefs.IsNotFound(err) {
			t.Errorf("expected NotFound error, got: %v", err)
		}
	})
}

// TestParallelUnpackSimulation simulates containerd's parallel layer unpacking
// where multiple layers are prepared and committed concurrently.
// This test verifies the snapshotter handles the race conditions correctly.
func TestParallelUnpackSimulation(t *testing.T) {
	s := newTestSnapshotter(t)
	ctx := t.Context()

	// Simulate a 5-layer image being unpacked in parallel
	// Layer order: layer0 (base) -> layer1 -> layer2 -> layer3 -> layer4
	const numLayers = 5

	// Track which layers are committed
	committed := make([]atomic.Bool, numLayers)

	// Error channel to collect errors from goroutines
	errCh := make(chan error, numLayers*2)

	var wg sync.WaitGroup

	// Each layer runs in its own goroutine, simulating parallel unpack
	for i := range numLayers {
		wg.Add(1)
		go func(layerIdx int) {
			defer wg.Done()

			prepareKey := fmt.Sprintf("default/%d/extract-%d", layerIdx, time.Now().UnixNano())
			commitKey := fmt.Sprintf("layer-%d", layerIdx)

			var parent string
			if layerIdx > 0 {
				parent = fmt.Sprintf("layer-%d", layerIdx-1)
			}

			// Retry loop - simulates containerd's retry on ErrNotFound
			const maxRetries = 50
			var lastErr error

			for attempt := range maxRetries {
				// Check if parent is committed (if we have one)
				if layerIdx > 0 && !committed[layerIdx-1].Load() {
					// Parent not ready - in real containerd this triggers retry
					time.Sleep(10 * time.Millisecond)
					continue
				}

				_, err := s.Prepare(ctx, prepareKey, parent)
				if err != nil {
					// Both NotFound and InvalidArgument indicate parent isn't ready:
					// - NotFound: parent doesn't exist yet
					// - InvalidArgument: parent exists but isn't committed
					if errdefs.IsNotFound(err) || errdefs.IsInvalidArgument(err) {
						// Parent not ready - retry
						lastErr = err
						time.Sleep(10 * time.Millisecond)
						continue
					}
					errCh <- fmt.Errorf("layer %d prepare (attempt %d): %w", layerIdx, attempt, err)
					return
				}

				// Simulate some work (unpacking layer content)
				time.Sleep(time.Duration(5+layerIdx*2) * time.Millisecond)

				// Commit the layer
				if err := s.Commit(ctx, commitKey, prepareKey); err != nil {
					errCh <- fmt.Errorf("layer %d commit: %w", layerIdx, err)
					return
				}

				// Mark as committed
				committed[layerIdx].Store(true)
				return
			}

			if lastErr != nil {
				errCh <- fmt.Errorf("layer %d exhausted retries, last error: %w", layerIdx, lastErr)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	// Collect all errors
	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("error: %v", err)
		}
		t.FailNow()
	}

	// Verify all layers were committed
	for i := range numLayers {
		if !committed[i].Load() {
			t.Errorf("layer %d was not committed", i)
		}
	}

	// Verify snapshots exist and have correct parent relationships
	for i := range numLayers {
		key := fmt.Sprintf("layer-%d", i)
		info, err := s.Stat(ctx, key)
		if err != nil {
			t.Errorf("stat layer %d: %v", i, err)
			continue
		}

		expectedParent := ""
		if i > 0 {
			expectedParent = fmt.Sprintf("layer-%d", i-1)
		}

		if info.Parent != expectedParent {
			t.Errorf("layer %d: expected parent %q, got %q", i, expectedParent, info.Parent)
		}
	}
}

// TestParallelUnpackStress runs many concurrent unpack simulations to stress
// test the snapshotter's handling of parallel operations.
func TestParallelUnpackStress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	s := newTestSnapshotter(t)
	ctx := t.Context()

	// Run multiple "image pulls" concurrently, each with their own layer chain
	const numImages = 5
	const layersPerImage = 4

	var wg sync.WaitGroup
	errCh := make(chan error, numImages*layersPerImage)

	for img := range numImages {
		wg.Add(1)
		go func(imageIdx int) {
			defer wg.Done()

			committed := make([]atomic.Bool, layersPerImage)

			var layerWg sync.WaitGroup
			for layer := range layersPerImage {
				layerWg.Add(1)
				go func(layerIdx int) {
					defer layerWg.Done()

					prepareKey := fmt.Sprintf("img%d/%d/extract-%d", imageIdx, layerIdx, time.Now().UnixNano())
					commitKey := fmt.Sprintf("img%d-layer%d", imageIdx, layerIdx)

					var parent string
					if layerIdx > 0 {
						parent = fmt.Sprintf("img%d-layer%d", imageIdx, layerIdx-1)
					}

					const maxRetries = 100
					for attempt := range maxRetries {
						// Wait for parent if needed
						if layerIdx > 0 && !committed[layerIdx-1].Load() {
							time.Sleep(5 * time.Millisecond)
							continue
						}

						_, err := s.Prepare(ctx, prepareKey, parent)
						if err != nil {
							// Both NotFound and InvalidArgument indicate parent isn't ready
							if errdefs.IsNotFound(err) || errdefs.IsInvalidArgument(err) {
								time.Sleep(5 * time.Millisecond)
								continue
							}
							errCh <- fmt.Errorf("img%d layer%d prepare (attempt %d): %w", imageIdx, layerIdx, attempt, err)
							return
						}

						// Simulate work
						time.Sleep(time.Duration(2+layerIdx) * time.Millisecond)

						if err := s.Commit(ctx, commitKey, prepareKey); err != nil {
							errCh <- fmt.Errorf("img%d layer%d commit: %w", imageIdx, layerIdx, err)
							return
						}

						committed[layerIdx].Store(true)
						return
					}

					errCh <- fmt.Errorf("img%d layer%d exhausted retries", imageIdx, layerIdx)
				}(layer)
			}
			layerWg.Wait()
		}(img)
	}

	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("error: %v", err)
		}
		t.FailNow()
	}

	// Verify all snapshots exist
	expectedCount := numImages * layersPerImage
	var actualCount int
	err := s.Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		if info.Kind == snapshots.KindCommitted {
			actualCount++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if actualCount != expectedCount {
		t.Errorf("expected %d committed snapshots, got %d", expectedCount, actualCount)
	}
}

// TestParentNotCommittedError verifies the custom error type works correctly.
func TestParentNotCommittedError(t *testing.T) {
	err := &ParentNotCommittedError{Parent: "test-parent"}

	// Should match errdefs.ErrNotFound
	if !errors.Is(err, errdefs.ErrNotFound) {
		t.Error("ParentNotCommittedError should match errdefs.ErrNotFound")
	}

	// Should have descriptive message
	msg := err.Error()
	if msg == "" {
		t.Error("error message should not be empty")
	}

	// errdefs.IsNotFound should return true
	if !errdefs.IsNotFound(err) {
		t.Error("errdefs.IsNotFound should return true for ParentNotCommittedError")
	}
}

// TestConcurrentPrepareWithParent verifies concurrent Prepare calls with
// the same parent don't cause races.
func TestConcurrentPrepareWithParent(t *testing.T) {
	s := newTestSnapshotter(t)
	ctx := t.Context()

	// Create and commit a base layer
	_, err := s.Prepare(ctx, "shared-base-prep", "")
	if err != nil {
		t.Fatalf("prepare base: %v", err)
	}
	if err := s.Commit(ctx, "shared-base", "shared-base-prep"); err != nil {
		t.Fatalf("commit base: %v", err)
	}

	// Now prepare many children concurrently from the same parent
	const numChildren = 20
	var wg sync.WaitGroup
	errCh := make(chan error, numChildren)

	for i := range numChildren {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("child-%d", idx)
			_, err := s.Prepare(ctx, key, "shared-base")
			if err != nil {
				errCh <- fmt.Errorf("child %d: %w", idx, err)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("error: %v", err)
	}

	// Verify all children were created
	var count int
	err = s.Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// Should have base + numChildren snapshots
	expected := 1 + numChildren
	if count != expected {
		t.Errorf("expected %d snapshots, got %d", expected, count)
	}
}

// TestRapidPrepareCommitCycles tests rapid prepare/commit cycles to catch
// any race conditions in the snapshot lifecycle.
func TestRapidPrepareCommitCycles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	s := newTestSnapshotter(t)
	ctx := t.Context()

	const numCycles = 50
	var wg sync.WaitGroup
	errCh := make(chan error, numCycles)

	for i := range numCycles {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			prepKey := fmt.Sprintf("rapid-%d-prep", idx)
			commitKey := fmt.Sprintf("rapid-%d", idx)

			_, err := s.Prepare(ctx, prepKey, "")
			if err != nil {
				errCh <- fmt.Errorf("prepare %d: %w", idx, err)
				return
			}

			if err := s.Commit(ctx, commitKey, prepKey); err != nil {
				errCh <- fmt.Errorf("commit %d: %w", idx, err)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("error: %v", err)
	}
}
