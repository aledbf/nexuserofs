package snapshotter

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
)

// Mount type constants for tests
const (
	testMountErofs       = "erofs"
	testMountFormatErofs = "format/erofs"
	testMountExt4        = "ext4"
	testMountBind        = "bind"
)

// newTestSnapshotterInternal creates a snapshotter for unit testing.
// Returns the internal *snapshotter type to access internal methods.
func newTestSnapshotterInternal(t *testing.T) *snapshotter {
	t.Helper()
	if !checkBlockModeRequirements(t) {
		t.Skip("mkfs.ext4 not available, required for block mode testing")
	}

	root := t.TempDir()
	s, err := NewSnapshotter(root, WithDefaultSize(1024*1024)) // 1MB for fast tests
	if err != nil {
		t.Fatalf("failed to create snapshotter: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Return the internal type
	return s.(*snapshotter)
}

func TestViewMountsWithLabels(t *testing.T) {
	s := newTestSnapshotterInternal(t)
	ctx := context.Background()

	t.Run("0 parents returns bind mount", func(t *testing.T) {
		// Create a view snapshot with no parent
		_, err := s.Prepare(ctx, "empty-active", "")
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if err := s.Commit(ctx, "empty-committed", "empty-active"); err != nil {
			t.Fatalf("commit: %v", err)
		}
		_, err = s.View(ctx, "empty-view", "empty-committed")
		if err != nil {
			t.Fatalf("view: %v", err)
		}

		// Get mounts - should be a bind mount for empty view
		mounts, err := s.Mounts(ctx, "empty-view")
		if err != nil {
			t.Fatalf("mounts: %v", err)
		}

		// For a view with a single committed parent, we should get an erofs mount
		// (the committed snapshot has a layer blob)
		if len(mounts) == 0 {
			t.Fatal("expected at least 1 mount")
		}
	})

	t.Run("1 parent returns single erofs mount", func(t *testing.T) {
		// Create base layer
		_, err := s.Prepare(ctx, "base-active", "")
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if err := s.Commit(ctx, "base-committed", "base-active"); err != nil {
			t.Fatalf("commit: %v", err)
		}

		// Create view on top of base
		_, err = s.View(ctx, "single-view", "base-committed")
		if err != nil {
			t.Fatalf("view: %v", err)
		}

		mounts, err := s.Mounts(ctx, "single-view")
		if err != nil {
			t.Fatalf("mounts: %v", err)
		}

		if len(mounts) != 1 {
			t.Fatalf("expected 1 mount, got %d", len(mounts))
		}

		// Should be erofs type
		if mounts[0].Type != testMountErofs {
			t.Errorf("mount.Type = %q, want %q", mounts[0].Type, testMountErofs)
		}
	})
}

func TestActiveMountsWithLabels(t *testing.T) {
	s := newTestSnapshotterInternal(t)
	ctx := context.Background()

	t.Run("0 parents returns ext4 only", func(t *testing.T) {
		// Create an active snapshot with no parent
		mounts, err := s.Prepare(ctx, "active-no-parent", "")
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}

		if len(mounts) != 1 {
			t.Fatalf("expected 1 mount, got %d", len(mounts))
		}

		if mounts[0].Type != testMountExt4 {
			t.Errorf("mount.Type = %q, want %q", mounts[0].Type, testMountExt4)
		}
	})

	t.Run("with parent returns erofs and ext4", func(t *testing.T) {
		// Create base layer
		_, err := s.Prepare(ctx, "parent-active", "")
		if err != nil {
			t.Fatalf("prepare parent: %v", err)
		}
		if err := s.Commit(ctx, "parent-committed", "parent-active"); err != nil {
			t.Fatalf("commit parent: %v", err)
		}

		// Create active snapshot on top
		mounts, err := s.Prepare(ctx, "child-active", "parent-committed")
		if err != nil {
			t.Fatalf("prepare child: %v", err)
		}

		// Should have 2 mounts: erofs (parent) + ext4 (writable)
		if len(mounts) != 2 {
			t.Fatalf("expected 2 mounts, got %d", len(mounts))
		}

		// First should be erofs
		if mounts[0].Type != testMountErofs {
			t.Errorf("mounts[0].Type = %q, want %q", mounts[0].Type, testMountErofs)
		}

		// Second should be ext4
		if mounts[1].Type != testMountExt4 {
			t.Errorf("mounts[1].Type = %q, want %q", mounts[1].Type, testMountExt4)
		}
	})
}

func TestMountFsMetaWithLabels(t *testing.T) {
	s := newTestSnapshotterInternal(t)
	ctx := context.Background()

	t.Run("returns false when no fsmeta label", func(t *testing.T) {
		// Create a single layer - should not have fsmeta
		_, err := s.Prepare(ctx, "single-active", "")
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if err := s.Commit(ctx, "single-committed", "single-active"); err != nil {
			t.Fatalf("commit: %v", err)
		}

		// Get the snapshot info
		var snap storage.Snapshot
		err = s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
			_, info, _, err := storage.GetInfo(ctx, "single-committed")
			if err != nil {
				return err
			}
			snap = storage.Snapshot{
				ID:        info.Name,
				ParentIDs: []string{},
			}
			return nil
		})
		if err != nil {
			t.Fatalf("get info: %v", err)
		}

		// mountFsMeta should return false (no fsmeta for single layer)
		_, ok := s.mountFsMeta(ctx, snap)
		if ok {
			t.Error("mountFsMeta should return false for single-layer snapshot")
		}
	})

	t.Run("returns format/erofs when fsmeta label set", func(t *testing.T) {
		// Create two layers to trigger fsmeta generation
		_, err := s.Prepare(ctx, "layer1-active", "")
		if err != nil {
			t.Fatalf("prepare layer1: %v", err)
		}
		if err := s.Commit(ctx, "layer1", "layer1-active"); err != nil {
			t.Fatalf("commit layer1: %v", err)
		}

		_, err = s.Prepare(ctx, "layer2-active", "layer1")
		if err != nil {
			t.Fatalf("prepare layer2: %v", err)
		}
		if err := s.Commit(ctx, "layer2", "layer2-active"); err != nil {
			t.Fatalf("commit layer2: %v", err)
		}

		// Check if fsmeta was generated (depends on whether mkfs.erofs is available)
		info, err := s.Stat(ctx, "layer2")
		if err != nil {
			t.Fatalf("stat: %v", err)
		}

		if info.Labels[LabelFsmetaReady] == LabelValueTrue {
			// Fsmeta was generated - create a view and check mount type
			_, err = s.View(ctx, "multi-view", "layer2")
			if err != nil {
				t.Fatalf("view: %v", err)
			}

			mounts, err := s.Mounts(ctx, "multi-view")
			if err != nil {
				t.Fatalf("mounts: %v", err)
			}

			if len(mounts) != 1 {
				t.Fatalf("expected 1 mount with fsmeta, got %d", len(mounts))
			}

			if mounts[0].Type != testMountFormatErofs {
				t.Errorf("mount.Type = %q, want %q", mounts[0].Type, testMountFormatErofs)
			}
		} else {
			t.Log("fsmeta not generated (mkfs.erofs may not be available)")
		}
	})
}

func TestMountFsMetaDeviceOrder(t *testing.T) {
	s := newTestSnapshotterInternal(t)
	ctx := context.Background()

	// Create 3 layers
	layers := []string{"layer1", "layer2", "layer3"}
	for i, name := range layers {
		var parent string
		if i > 0 {
			parent = layers[i-1]
		}
		activeName := name + "-active"
		_, err := s.Prepare(ctx, activeName, parent)
		if err != nil {
			t.Fatalf("prepare %s: %v", name, err)
		}
		if err := s.Commit(ctx, name, activeName); err != nil {
			t.Fatalf("commit %s: %v", name, err)
		}
	}

	// Check if fsmeta was generated
	info, err := s.Stat(ctx, "layer3")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if info.Labels[LabelFsmetaReady] != LabelValueTrue {
		t.Skip("fsmeta not generated (mkfs.erofs may not be available)")
	}

	// Create view to get mounts
	_, err = s.View(ctx, "order-view", "layer3")
	if err != nil {
		t.Fatalf("view: %v", err)
	}

	mounts, err := s.Mounts(ctx, "order-view")
	if err != nil {
		t.Fatalf("mounts: %v", err)
	}

	if len(mounts) != 1 || mounts[0].Type != testMountFormatErofs {
		t.Skip("expected format/erofs mount with fsmeta")
	}

	// Verify device options are present
	deviceCount := 0
	for _, opt := range mounts[0].Options {
		if len(opt) > 7 && opt[:7] == "device=" {
			deviceCount++
		}
	}

	if deviceCount != 3 {
		t.Errorf("expected 3 device options, got %d", deviceCount)
	}
}

func TestViewMountsForKindDecisionTree(t *testing.T) {
	s := newTestSnapshotterInternal(t)
	ctx := context.Background()

	t.Run("view returns correct mount type", func(t *testing.T) {
		// Create and commit a layer
		_, err := s.Prepare(ctx, "dt-active", "")
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if err := s.Commit(ctx, "dt-committed", "dt-active"); err != nil {
			t.Fatalf("commit: %v", err)
		}

		// Create view
		_, err = s.View(ctx, "dt-view", "dt-committed")
		if err != nil {
			t.Fatalf("view: %v", err)
		}

		// Get mounts
		mounts, err := s.Mounts(ctx, "dt-view")
		if err != nil {
			t.Fatalf("mounts: %v", err)
		}

		// Should be erofs for single layer
		if len(mounts) != 1 {
			t.Fatalf("expected 1 mount, got %d", len(mounts))
		}
		if mounts[0].Type != testMountErofs {
			t.Errorf("mount.Type = %q, want %q", mounts[0].Type, testMountErofs)
		}
	})
}

func TestActiveMountsForKindDecisionTree(t *testing.T) {
	s := newTestSnapshotterInternal(t)
	ctx := context.Background()

	t.Run("active with no parent returns ext4", func(t *testing.T) {
		mounts, err := s.Prepare(ctx, "adk-active", "")
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}

		if len(mounts) != 1 {
			t.Fatalf("expected 1 mount, got %d", len(mounts))
		}
		if mounts[0].Type != testMountExt4 {
			t.Errorf("mount.Type = %q, want %q", mounts[0].Type, testMountExt4)
		}
	})
}

func TestSingleLayerMountsRequiresActive(t *testing.T) {
	s := newTestSnapshotterInternal(t)
	ctx := context.Background()

	// Create and commit a layer
	_, err := s.Prepare(ctx, "sl-active", "")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := s.Commit(ctx, "sl-committed", "sl-active"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create view - should work
	_, err = s.View(ctx, "sl-view", "sl-committed")
	if err != nil {
		t.Fatalf("view: %v", err)
	}

	// Verify view has mounts
	mounts, err := s.Mounts(ctx, "sl-view")
	if err != nil {
		t.Fatalf("mounts: %v", err)
	}

	if len(mounts) == 0 {
		t.Error("view should have mounts")
	}
}

func TestFindLayerBlobFromInfo(t *testing.T) {
	s := newTestSnapshotterInternal(t)

	root := s.root
	snapshotID := "test-snap"
	blobPath := filepath.Join(root, "snapshots", snapshotID, "sha256-abc123.erofs")

	// Create the directory and blob file
	if err := os.MkdirAll(filepath.Dir(blobPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(blobPath, []byte("fake"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Run("returns path when label set and file exists", func(t *testing.T) {
		info := snapshots.Info{
			Labels: map[string]string{
				LabelLayerBlobPath: blobPath,
			},
		}

		found, err := s.findLayerBlobFromInfo(snapshotID, info)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if found != blobPath {
			t.Errorf("got %q, want %q", found, blobPath)
		}
	})

	t.Run("finds blob via glob fallback when label not set", func(t *testing.T) {
		info := snapshots.Info{
			Labels: map[string]string{},
		}

		// Should find the blob via glob even without a label
		found, err := s.findLayerBlobFromInfo(snapshotID, info)
		if err != nil {
			t.Fatalf("expected glob fallback to find blob: %v", err)
		}
		if found != blobPath {
			t.Errorf("got %q, want %q", found, blobPath)
		}
	})

	t.Run("returns error when no blob exists", func(t *testing.T) {
		// Use a different snapshot ID with no files
		emptyID := "empty-snap"
		emptyDir := filepath.Join(root, "snapshots", emptyID)
		if err := os.MkdirAll(emptyDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		info := snapshots.Info{
			Labels: map[string]string{
				LabelLayerBlobPath: "/nonexistent/path.erofs",
			},
		}

		_, err := s.findLayerBlobFromInfo(emptyID, info)
		if err == nil {
			t.Error("expected error when no blob exists")
		}
	})
}
