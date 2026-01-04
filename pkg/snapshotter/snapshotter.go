/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package erofs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/containerd/continuity/fs"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/moby/sys/mountinfo"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/aledbf/nexuserofs/internal/fsverity"
)

// SnapshotterConfig is used to configure the erofs snapshotter instance
type SnapshotterConfig struct {
	// ovlOptions are the base options added to the overlayfs mount (defaults to [""])
	ovlOptions []string
	// enableFsverity enables fsverity for EROFS layers
	enableFsverity bool
	// setImmutable enables IMMUTABLE_FL file attribute for EROFS layers
	setImmutable bool
	// defaultSize creates a default size writable layer for active snapshots
	defaultSize int64
	// fsMergeThreshold (>0) enables fsmerge when the number of image layers exceeds this value
	fsMergeThreshold uint
}

// Opt is an option to configure the erofs snapshotter
type Opt func(config *SnapshotterConfig)

// WithOvlOptions defines the extra mount options for overlayfs
func WithOvlOptions(options []string) Opt {
	return func(config *SnapshotterConfig) {
		config.ovlOptions = options
	}
}

// WithFsverity enables fsverity for EROFS layers
func WithFsverity() Opt {
	return func(config *SnapshotterConfig) {
		config.enableFsverity = true
	}
}

// WithImmutable enables IMMUTABLE_FL file attribute for EROFS layers
func WithImmutable() Opt {
	return func(config *SnapshotterConfig) {
		config.setImmutable = true
	}
}

// WithDefaultSize creates a default size writable layer for active snapshots
func WithDefaultSize(size int64) Opt {
	return func(config *SnapshotterConfig) {
		config.defaultSize = size
	}
}

// WithFsMergeThreshold (>0) enables fsmerge when the number of image layers exceeds this value
func WithFsMergeThreshold(v uint) Opt {
	return func(config *SnapshotterConfig) {
		config.fsMergeThreshold = v
	}
}

type MetaStore interface {
	TransactionContext(ctx context.Context, writable bool) (context.Context, storage.Transactor, error)
	WithTransaction(ctx context.Context, writable bool, fn storage.TransactionCallback) error
	Close() error
}

type snapshotter struct {
	root             string
	ms               *storage.MetaStore
	ovlOptions       []string
	enableFsverity   bool
	setImmutable     bool
	defaultWritable  int64
	blockMode        bool
	fsMergeThreshold uint
}

const (
	// extractLabel is the label key used to mark snapshots for layer extraction.
	// This is stored in the snapshot metadata for atomic reads within transactions,
	// avoiding TOCTOU race conditions that would occur with filesystem markers.
	//
	// TOCTOU Safety: The label is set atomically within a database transaction
	// during CreateSnapshot, and all reads occur within transactions. This ensures
	// no race window exists between checking and using the extract status.
	extractLabel = "containerd.io/snapshot/erofs.extract"

	// erofsLayerMarker is a filesystem marker file that indicates a directory
	// is managed by the EROFS snapshotter.
	//
	// Purpose: The EROFS differ (plugins/diff/erofs) checks for this marker
	// to validate that mounts are genuine EROFS snapshotter layers before
	// processing them. Without this marker, the differ returns ErrNotImplemented,
	// allowing fallback to other differs.
	//
	// Creation points: The marker is created via ensureMarkerFile() in multiple
	// code paths because different operations may create the layer directory:
	//   - prepareDirectory: Initial snapshot creation (Prepare/View)
	//   - activeMounts: When returning mounts for an active snapshot
	//   - diffMounts: When preparing mounts specifically for diff operations
	//
	// TOCTOU Safety: ensureMarkerFile() uses O_CREAT|O_EXCL for atomic creation,
	// eliminating the race window between existence check and file creation.
	// The function is idempotent - concurrent calls safely succeed without errors.
	// While the marker file check in the differ can theoretically race with cleanup,
	// the extractLabel (database-backed) is the authoritative source for extract
	// status decisions, with the marker serving as a validation hint.
	erofsLayerMarker = ".erofslayer"
)

// NewSnapshotter returns a Snapshotter which uses EROFS+OverlayFS. The layers
// are stored under the provided root. A metadata file is stored under the root.
func NewSnapshotter(root string, opts ...Opt) (snapshots.Snapshotter, error) {
	config := SnapshotterConfig{
		defaultSize: defaultWritableSize,
	}
	for _, opt := range opts {
		opt(&config)
	}

	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}

	if config.defaultSize == 0 {
		// If not block mode, check root compatibility
		if err := checkCompatibility(root); err != nil {
			return nil, err
		}
	}

	// Check fsverity support if enabled
	if config.enableFsverity {
		// TODO: Call specific function here
		supported, err := fsverity.IsSupported(root)
		if err != nil {
			return nil, fmt.Errorf("failed to check fsverity support on %q: %w", root, err)
		}
		if !supported {
			return nil, fmt.Errorf("fsverity is not supported on the filesystem of %q", root)
		}
	}

	if config.setImmutable && runtime.GOOS != "linux" {
		return nil, fmt.Errorf("setting IMMUTABLE_FL is only supported on Linux")
	}

	ms, err := storage.NewMetaStore(filepath.Join(root, "metadata.db"))
	if err != nil {
		return nil, err
	}

	if err := os.Mkdir(filepath.Join(root, "snapshots"), 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}

	return &snapshotter{
		root:             root,
		ms:               ms,
		ovlOptions:       config.ovlOptions,
		enableFsverity:   config.enableFsverity,
		setImmutable:     config.setImmutable,
		defaultWritable:  config.defaultSize,
		blockMode:        config.defaultSize > 0,
		fsMergeThreshold: config.fsMergeThreshold,
	}, nil
}

// Close closes the snapshotter
func (s *snapshotter) Close() error {
	return s.ms.Close()
}

func (s *snapshotter) upperPath(id string) string {
	return filepath.Join(s.root, "snapshots", id, "fs")
}

func (s *snapshotter) upperDir(id string) string {
	if s.blockMode {
		return filepath.Join(s.upperPath(id), "rw", "upper")
	}
	return s.upperPath(id)
}

func (s *snapshotter) workDir(id string) string {
	if s.blockMode {
		return filepath.Join(s.upperPath(id), "rw", "work")
	}
	return s.workPath(id)
}

func (s *snapshotter) workPath(id string) string {
	return filepath.Join(s.root, "snapshots", id, "work")
}

func (s *snapshotter) writablePath(id string) string {
	return filepath.Join(s.root, "snapshots", id, "rwlayer.img")
}

// createWritableLayer creates and formats an ext4 filesystem image file.
// This is called during Prepare() to eagerly create the writable layer,
// avoiding the need for lazy mkfs/ext4 mount type processing.
// The upper/work directories are created by the mount manager when mounting.
func (s *snapshotter) createWritableLayer(ctx context.Context, id string) error {
	path := s.writablePath(id)
	// TODO: Get size from snapshot labels to allow per-container custom sizes
	size := s.defaultWritable

	// Create sparse file
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create writable layer file: %w", err)
	}

	if err := f.Truncate(size); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("failed to allocate writable layer: %w", err)
	}
	f.Close()

	// Format as ext4 directly on the file (mkfs.ext4 supports this).
	// Use lazy_itable_init and lazy_journal_init to defer initialization
	// to the background, significantly speeding up mkfs for large sparse files.
	cmd := exec.CommandContext(ctx, "mkfs.ext4", "-q", "-F", "-L", "rwlayer",
		"-E", "nodiscard,lazy_itable_init=1,lazy_journal_init=1", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(path)
		return fmt.Errorf("failed to format ext4: %w: %s", err, truncateOutput(out, 256))
	}

	log.G(ctx).WithField("path", path).WithField("size", size).Debug("created writable layer")
	return nil
}

// A committed layer blob generated by the EROFS differ
func (s *snapshotter) layerBlobPath(id string) string {
	return filepath.Join(s.root, "snapshots", id, "layer.erofs")
}

func (s *snapshotter) fsMetaPath(id string) string {
	return filepath.Join(s.root, "snapshots", id, "fsmeta.erofs")
}

func (s *snapshotter) lowerPath(id string) (string, error) {
	layerBlob := s.layerBlobPath(id)
	if _, err := os.Stat(layerBlob); err != nil {
		return "", fmt.Errorf("failed to find valid erofs layer blob: %w", err)
	}

	return layerBlob, nil
}

func (s *snapshotter) prepareDirectory(ctx context.Context, snapshotDir string, kind snapshots.Kind) (string, error) {
	td, err := os.MkdirTemp(snapshotDir, "new-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	if err := os.Mkdir(filepath.Join(td, "fs"), 0755); err != nil {
		return td, err
	}
	if kind == snapshots.KindActive {
		if !s.blockMode {
			if err := os.Mkdir(filepath.Join(td, "work"), 0711); err != nil {
				return td, err
			}
		}
		// Create EROFS layer marker at snapshot root (e.g., /snapshots/{id}/.erofslayer).
		// This is the primary marker location checked by the differ for bind/overlay mounts.
		// Uses ensureMarkerFile for atomic creation consistent with other code paths.
		if err := ensureMarkerFile(filepath.Join(td, erofsLayerMarker)); err != nil {
			return td, err
		}
	}

	return td, nil
}

func (s *snapshotter) mountFsMeta(snap storage.Snapshot, id int) (mount.Mount, bool) {
	if s.blockMode {
		return mount.Mount{}, false
	}

	mergedMeta := s.fsMetaPath(snap.ParentIDs[id])
	if fi, err := os.Stat(mergedMeta); err != nil || fi.Size() == 0 {
		return mount.Mount{}, false
	}

	m := mount.Mount{
		Source:  mergedMeta,
		Type:    "erofs",
		Options: []string{"ro", "loop"},
	}
	for j := len(snap.ParentIDs) - 1; j >= id; j-- {
		blob := s.layerBlobPath(snap.ParentIDs[j])
		fi, err := os.Stat(blob)
		if err != nil || fi.Size() == 0 {
			return mount.Mount{}, false
		}
		m.Options = append(m.Options, "device="+blob)
	}
	return m, true
}

// mounts returns mount specifications for a snapshot.
// For blockMode active snapshots, it performs actual mounting via activeMounts.
// For other cases, it returns template-based mount specs for the mount manager.
func (s *snapshotter) mounts(snap storage.Snapshot, info snapshots.Info) ([]mount.Mount, error) {
	if s.blockMode && snap.Kind == snapshots.KindActive {
		if isExtractSnapshot(info) {
			return s.diffMounts(snap)
		}
		return s.templateMounts(snap)
	}
	return s.templateMounts(snap)
}

// runtimeMounts returns mount specifications for an already-prepared snapshot.
// Unlike mounts(), it never calls activeMounts() since the snapshot is already set up.
func (s *snapshotter) runtimeMounts(snap storage.Snapshot, info snapshots.Info) ([]mount.Mount, error) {
	if s.blockMode && snap.Kind == snapshots.KindActive && isExtractSnapshot(info) {
		return s.diffMounts(snap)
	}
	return s.templateMounts(snap)
}

// isExtractSnapshot returns true if the snapshot is marked for layer extraction.
// This is determined by the extractLabel in the snapshot metadata, which is set
// atomically during snapshot creation for TOCTOU safety.
func isExtractSnapshot(info snapshots.Info) bool {
	return info.Labels[extractLabel] == "true"
}

// templateMounts builds mount specifications using templates for the mount manager.
// This is the common implementation used by both mounts() and runtimeMounts().
func (s *snapshotter) templateMounts(snap storage.Snapshot) ([]mount.Mount, error) {
	var options []string

	if len(snap.ParentIDs) == 0 {
		return s.singleLayerMounts(snap, options)
	}

	var mounts []mount.Mount
	if snap.Kind == snapshots.KindActive {
		mounts, options = s.activeLayerMounts(snap, options)
	} else if len(snap.ParentIDs) == 1 {
		// Single parent view - return EROFS mount directly
		layerBlob, err := s.lowerPath(snap.ParentIDs[0])
		if err != nil {
			return nil, err
		}
		return []mount.Mount{
			{
				Source:  layerBlob,
				Type:    "erofs",
				Options: []string{"ro", "loop"},
			},
		}, nil
	}

	// Build lower layer mounts
	first := len(mounts)
	for i := range snap.ParentIDs {
		if s.fsMergeThreshold > 0 {
			if m, ok := s.mountFsMeta(snap, i); ok {
				mounts = append(mounts, m)
				first = len(mounts) - 1
				break
			}
		}

		layerBlob, err := s.lowerPath(snap.ParentIDs[i])
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, mount.Mount{
			Source:  layerBlob,
			Type:    "erofs",
			Options: []string{"ro", "loop"},
		})
	}

	// Build overlay options
	if (len(mounts) - first) == 1 {
		if snap.Kind == snapshots.KindView {
			return mounts, nil
		}
		options = append(options, fmt.Sprintf("lowerdir={{ mount %d }}", first))
	} else {
		options = append(options, fmt.Sprintf("lowerdir={{ overlay %d %d }}", first, len(mounts)-1))
	}
	if snap.Kind == snapshots.KindView {
		options = append(options, "ro")
	}
	options = append(options, s.ovlOptions...)

	return append(mounts, mount.Mount{
		Type:    "format/mkdir/overlay",
		Source:  "overlay",
		Options: options,
	}), nil
}

// singleLayerMounts returns mounts for a snapshot with no parent layers.
func (s *snapshotter) singleLayerMounts(snap storage.Snapshot, options []string) ([]mount.Mount, error) {
	// Check if this is a committed layer
	if layerBlob, err := s.lowerPath(snap.ID); err == nil {
		if snap.Kind != snapshots.KindView {
			return nil, fmt.Errorf("only works for snapshots.KindView on a committed snapshot: %w", err)
		}
		if s.enableFsverity {
			if err := s.verifyFsverity(layerBlob); err != nil {
				return nil, err
			}
		}
		return []mount.Mount{
			{
				Source:  layerBlob,
				Type:    "erofs",
				Options: []string{"ro", "loop"},
			},
		}, nil
	}

	// No parents and no committed layer - return bind mount
	roFlag := "rw"
	if snap.Kind == snapshots.KindView {
		roFlag = "ro"
	}

	if s.blockMode {
		writablePath := s.writablePath(snap.ID)

		// Check if the writable layer was already created by createWritableLayer()
		// during Prepare(). If so, use ext4 type directly. Otherwise, use mkfs/ext4
		// to have the mount manager create and format it (lazy creation fallback).
		var writableMount mount.Mount
		if _, err := os.Stat(writablePath); err == nil {
			// File exists - already formatted by createWritableLayer()
			writableMount = mount.Mount{
				Source:  writablePath,
				Type:    "ext4",
				Options: []string{roFlag, "loop"},
			}
		} else {
			// File doesn't exist - use mkfs transformer for lazy creation
			writableMount = mount.Mount{
				Source: writablePath,
				Type:   "mkfs/ext4",
				Options: []string{
					"X-containerd.mkfs.fs=ext4",
					fmt.Sprintf("X-containerd.mkfs.size=%d", s.defaultWritable),
					roFlag,
					"loop",
				},
			}
		}

		return []mount.Mount{
			writableMount,
			{
				Source: "{{ mount 0 }}/upper",
				Type:   "format/mkdir/bind",
				Options: append(options,
					"X-containerd.mkdir.path={{ mount 0 }}/upper:0755",
					roFlag,
					"rbind",
				),
			},
		}, nil
	}

	return []mount.Mount{
		{
			Source: s.upperPath(snap.ID),
			Type:   "bind",
			Options: append(options,
				roFlag,
				"rbind",
			),
		},
	}, nil
}

// activeLayerMounts returns the initial mounts and options for an active snapshot.
func (s *snapshotter) activeLayerMounts(snap storage.Snapshot, options []string) ([]mount.Mount, []string) {
	var mounts []mount.Mount

	if s.blockMode {
		writablePath := s.writablePath(snap.ID)

		// Check if the writable layer was already created by createWritableLayer()
		// during Prepare(). If so, use ext4 type directly. Otherwise, use mkfs/ext4
		// to have the mount manager create and format it (lazy creation fallback).
		var m mount.Mount
		if _, err := os.Stat(writablePath); err == nil {
			// File exists - already formatted by createWritableLayer()
			m = mount.Mount{
				Source:  writablePath,
				Type:    "ext4",
				Options: []string{"rw", "loop"},
			}
		} else {
			// File doesn't exist - use mkfs transformer for lazy creation
			m = mount.Mount{
				Source: writablePath,
				Type:   "mkfs/ext4",
				Options: []string{
					"X-containerd.mkfs.fs=ext4",
					fmt.Sprintf("X-containerd.mkfs.size=%d", s.defaultWritable),
					"rw",
					"loop",
				},
			}
		}
		mounts = append(mounts, m)

		options = append(options,
			"X-containerd.mkdir.path={{ mount 0 }}/upper:0755",
			"X-containerd.mkdir.path={{ mount 0 }}/work:0755",
			"workdir={{ mount 0 }}/work",
			"upperdir={{ mount 0 }}/upper",
		)
	} else {
		options = append(options,
			fmt.Sprintf("workdir=%s", s.workPath(snap.ID)),
			fmt.Sprintf("upperdir=%s", s.upperPath(snap.ID)),
		)
	}

	return mounts, options
}

// isExtractKey returns true if the key indicates an extract/unpack operation.
// Snapshot keys use forward slashes as separators (e.g., "default/1/extract-12345"),
// so we use path.Base (POSIX paths) rather than filepath.Base (OS-specific).
func isExtractKey(key string) bool {
	return strings.HasPrefix(path.Base(key), snapshots.UnpackKeyPrefix)
}

// truncateOutput truncates command output to maxLen bytes for inclusion in error
// messages. This prevents verbose tool output from overwhelming error logs.
func truncateOutput(out []byte, maxLen int) string {
	if len(out) <= maxLen {
		return string(out)
	}
	return string(out[:maxLen]) + "... (truncated)"
}

// ensureMarkerFile creates the EROFS layer marker file at the given path if
// it doesn't already exist. This is idempotent - calling it multiple times
// with the same path is safe and will not return an error.
//
// The marker file is checked by erofsutils.MountsToLayer() in the EROFS differ
// to validate that a directory is a genuine EROFS snapshotter layer. If the
// marker is missing, the differ returns ErrNotImplemented to allow fallback
// to other differs (e.g., the walking differ).
//
// This uses O_CREAT|O_EXCL for atomic creation, avoiding TOCTOU races that
// would occur with a separate existence check followed by creation.
func ensureMarkerFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		if os.IsExist(err) {
			// File already exists, which is fine for idempotency
			return nil
		}
		return err
	}
	return f.Close()
}

func (s *snapshotter) activeMounts(snap storage.Snapshot) ([]mount.Mount, error) {
	upperRoot := s.upperPath(snap.ID)
	rwRoot := filepath.Join(upperRoot, "rw")
	upperDir := filepath.Join(rwRoot, "upper")
	workDir := filepath.Join(rwRoot, "work")
	mergedDir := filepath.Join(upperRoot, "merged")

	if err := os.MkdirAll(upperRoot, 0755); err != nil {
		return nil, fmt.Errorf("failed to create upper root: %w", err)
	}

	if err := os.MkdirAll(rwRoot, 0755); err != nil {
		return nil, fmt.Errorf("failed to create rw root: %w", err)
	}

	// Mount the writable layer if not already mounted
	if err := s.ensureWritableMount(snap.ID, rwRoot, upperRoot); err != nil {
		return nil, err
	}

	if err := s.ensureActiveDirectories(upperDir, workDir, mergedDir); err != nil {
		return nil, err
	}

	// Check if already fully mounted
	mergedMounted, err := mountinfo.Mounted(mergedDir)
	if err != nil {
		return nil, fmt.Errorf("failed to check merged mount: %w", err)
	}
	if mergedMounted {
		return []mount.Mount{
			{
				Type:    "bind",
				Source:  mergedDir,
				Options: []string{"rw", "rbind"},
			},
		}, nil
	}

	// Create EROFS layer markers in active mount subdirectories.
	// These markers at fs/ and fs/rw/ ensure the differ can validate layers
	// when mounts reference these directories (e.g., overlay upperdir paths).
	for _, markerRoot := range []string{upperRoot, rwRoot} {
		if err := ensureMarkerFile(filepath.Join(markerRoot, erofsLayerMarker)); err != nil {
			return nil, fmt.Errorf("failed to create erofs marker in %s: %w", markerRoot, err)
		}
	}

	if len(snap.ParentIDs) == 0 {
		return []mount.Mount{
			{
				Type:    "bind",
				Source:  upperDir,
				Options: []string{"rw", "rbind"},
			},
		}, nil
	}

	// Mount lower layers and overlay
	return s.mountOverlay(snap, upperRoot, upperDir, workDir, mergedDir)
}

// ensureWritableMount mounts the ext4 writable layer if not already mounted.
func (s *snapshotter) ensureWritableMount(id, rwRoot, upperRoot string) error {
	mounted, err := mountinfo.Mounted(rwRoot)
	if err != nil {
		return fmt.Errorf("failed to check rw root mount: %w", err)
	}
	if mounted {
		return nil
	}

	ext4Mount := mount.Mount{
		Source:  s.writablePath(id),
		Type:    "ext4",
		Options: []string{"rw", "loop"},
	}
	if err := mount.All([]mount.Mount{ext4Mount}, rwRoot); err != nil {
		return fmt.Errorf("failed to mount writable layer: %w", err)
	}
	return nil
}

// ensureActiveDirectories creates the upper, work, and merged directories.
func (s *snapshotter) ensureActiveDirectories(upperDir, workDir, mergedDir string) error {
	if err := os.MkdirAll(upperDir, 0755); err != nil {
		return fmt.Errorf("failed to create upper dir: %w", err)
	}
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return fmt.Errorf("failed to create work dir: %w", err)
	}
	if err := os.MkdirAll(mergedDir, 0755); err != nil {
		return fmt.Errorf("failed to create merged dir: %w", err)
	}
	return nil
}

// mountOverlay mounts the lower EROFS layers and creates the overlay mount.
func (s *snapshotter) mountOverlay(snap storage.Snapshot, upperRoot, upperDir, workDir, mergedDir string) ([]mount.Mount, error) {
	lowerMounts, err := s.collectLowerMounts(snap)
	if err != nil {
		return nil, err
	}

	lowerRoot := filepath.Join(upperRoot, "lower")
	lowerDirs, err := s.mountLowerLayers(lowerMounts, lowerRoot)
	if err != nil {
		return nil, err
	}

	options := []string{
		fmt.Sprintf("lowerdir=%s", strings.Join(lowerDirs, ":")),
		fmt.Sprintf("upperdir=%s", upperDir),
		fmt.Sprintf("workdir=%s", workDir),
	}
	options = append(options, s.ovlOptions...)

	overlay := mount.Mount{
		Type:    "overlay",
		Source:  "overlay",
		Options: options,
	}
	if err := mount.All([]mount.Mount{overlay}, mergedDir); err != nil {
		return nil, fmt.Errorf("failed to mount overlay: %w", err)
	}

	return []mount.Mount{
		{
			Type:    "bind",
			Source:  mergedDir,
			Options: []string{"rw", "rbind"},
		},
	}, nil
}

// collectLowerMounts collects the EROFS mount specifications for parent layers.
func (s *snapshotter) collectLowerMounts(snap storage.Snapshot) ([]mount.Mount, error) {
	var lowerMounts []mount.Mount
	for i := range snap.ParentIDs {
		if s.fsMergeThreshold > 0 {
			if m, ok := s.mountFsMeta(snap, i); ok {
				lowerMounts = append(lowerMounts, m)
				break
			}
		}
		layerBlob, err := s.lowerPath(snap.ParentIDs[i])
		if err != nil {
			return nil, err
		}
		lowerMounts = append(lowerMounts, mount.Mount{
			Source:  layerBlob,
			Type:    "erofs",
			Options: []string{"ro", "loop"},
		})
	}
	return lowerMounts, nil
}

// mountLowerLayers mounts each lower layer and returns the list of mount points.
func (s *snapshotter) mountLowerLayers(lowerMounts []mount.Mount, lowerRoot string) ([]string, error) {
	var lowerDirs []string
	for i, m := range lowerMounts {
		target := filepath.Join(lowerRoot, fmt.Sprintf("%d", i))
		if err := os.MkdirAll(target, 0755); err != nil {
			return nil, fmt.Errorf("failed to create lower dir: %w", err)
		}
		mounted, err := mountinfo.Mounted(target)
		if err != nil {
			return nil, fmt.Errorf("failed to check lower mount: %w", err)
		}
		if !mounted {
			if err := mount.All([]mount.Mount{m}, target); err != nil {
				return nil, fmt.Errorf("failed to mount lower layer: %w", err)
			}
		}
		lowerDirs = append(lowerDirs, target)
	}
	return lowerDirs, nil
}

func (s *snapshotter) diffMounts(snap storage.Snapshot) ([]mount.Mount, error) {
	upperRoot := s.upperPath(snap.ID)
	layerRoot := filepath.Dir(upperRoot)

	if err := os.MkdirAll(upperRoot, 0755); err != nil {
		return nil, fmt.Errorf("failed to create upper root: %w", err)
	}

	// Ensure EROFS layer marker exists at the snapshot root for diff operations.
	// This may be redundant with createSnapshotDirectory, but ensureMarkerFile
	// is idempotent and this guards against edge cases where diff mounts are
	// requested without a prior Prepare call.
	if err := ensureMarkerFile(filepath.Join(layerRoot, erofsLayerMarker)); err != nil {
		return nil, fmt.Errorf("failed to create erofs marker: %w", err)
	}

	return []mount.Mount{
		{
			Type:    "bind",
			Source:  upperRoot,
			Options: []string{"rw", "rbind"},
		},
	}, nil
}

func (s *snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) (_ []mount.Mount, err error) {
	var (
		snap     storage.Snapshot
		td, path string
		info     snapshots.Info
	)

	defer func() {
		if err != nil {
			if td != "" {
				if err1 := os.RemoveAll(td); err1 != nil {
					log.G(ctx).WithError(err1).Warn("failed to cleanup temp snapshot directory")
				}
			}
			if path != "" {
				if err1 := os.RemoveAll(path); err1 != nil {
					log.G(ctx).WithError(err1).WithField("path", path).Error("failed to reclaim snapshot directory, directory may need removal")
					err = fmt.Errorf("failed to remove path: %v: %w", err1, err)
				}
			}
		}
	}()

	snapshotDir := filepath.Join(s.root, "snapshots")
	td, err = s.prepareDirectory(ctx, snapshotDir, kind)
	if err != nil {
		return nil, fmt.Errorf("failed to create prepare snapshot dir: %w", err)
	}

	// Mark extract snapshots with a label for TOCTOU-safe detection.
	// This is done within the transaction so the label is atomically
	// associated with the snapshot metadata.
	if isExtractKey(key) {
		opts = append(opts, snapshots.WithLabels(map[string]string{
			extractLabel: "true",
		}))
	}

	if err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) (err error) {
		snap, err = storage.CreateSnapshot(ctx, kind, key, parent, opts...)
		if err != nil {
			return fmt.Errorf("failed to create snapshot: %w", err)
		}

		_, info, _, err = storage.GetInfo(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to get snapshot info: %w", err)
		}

		if len(snap.ParentIDs) > 0 {
			if err := upperDirectoryPermission(filepath.Join(td, "fs"), s.upperPath(snap.ParentIDs[0])); err != nil {
				return err
			}
		}

		path = filepath.Join(snapshotDir, snap.ID)
		if err = os.Rename(td, path); err != nil {
			return fmt.Errorf("failed to rename: %w", err)
		}
		td = ""
		return nil
	}); err != nil {
		return nil, err
	}

	// Generate fsmeta outside of the transaction since it's unnecessary.
	// Also ignore all errors since it's a nice-to-have stuff.
	if !isExtractKey(key) {
		s.generateFsMeta(ctx, snap.ParentIDs)
	}

	// For active snapshots in block mode, create the writable layer immediately.
	// This avoids the need for lazy mkfs/ext4 processing which requires a mount
	// manager and doesn't work well with VM-based runtimes that need the file
	// to exist before mounting.
	if kind == snapshots.KindActive && s.blockMode && !isExtractKey(key) {
		if err := s.createWritableLayer(ctx, snap.ID); err != nil {
			return nil, fmt.Errorf("failed to create writable layer: %w", err)
		}
	}

	return s.mounts(snap, info)
}

func (s *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return s.createSnapshot(ctx, snapshots.KindActive, key, parent, opts)
}

func (s *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return s.createSnapshot(ctx, snapshots.KindView, key, parent, opts)
}

func (s *snapshotter) commitBlock(ctx context.Context, layerBlob string, id string) error {
	layer := s.writablePath(id)
	if _, err := os.Stat(layer); err != nil {
		if os.IsNotExist(err) {
			if cerr := convertDirToErofs(ctx, layerBlob, s.upperPath(id)); cerr != nil {
				return fmt.Errorf("failed to convert upper to erofs layer: %w", cerr)
			}
			// TODO: Cleanup method?
			return nil
		}
		return fmt.Errorf("failed to access writable layer %s: %w", layer, err)
	}

	rwRoot := filepath.Join(s.upperPath(id), "rw")
	if err := os.MkdirAll(rwRoot, 0755); err != nil {
		return fmt.Errorf("failed to create rw root: %w", err)
	}

	// Check if already mounted (from Prepare) before trying to mount again.
	// If already mounted, we can use the existing mount.
	alreadyMounted, err := mountinfo.Mounted(rwRoot)
	if err != nil {
		return fmt.Errorf("failed to check mount status: %w", err)
	}
	if !alreadyMounted {
		m := mount.Mount{
			Source:  layer,
			Type:    "ext4",
			Options: []string{"ro", "loop", "noload"},
		}
		if err := m.Mount(rwRoot); err != nil {
			return fmt.Errorf("failed to mount writable layer %s: %w", layer, err)
		}
		log.G(ctx).WithField("target", rwRoot).Debug("Mounted writable layer for conversion")
	}

	// Always cleanup active mounts after conversion
	defer func() {
		if err := cleanupActiveMounts(s.upperPath(id)); err != nil {
			log.G(ctx).WithError(err).WithField("id", id).Warn("failed to cleanup active mounts after conversion")
		}
	}()

	upperDir := s.upperDir(id)
	if _, err := os.Stat(upperDir); os.IsNotExist(err) {
		// upper is empty, just convert the empty directory
		upperDir = s.upperPath(id)
	}
	if cerr := convertDirToErofs(ctx, layerBlob, upperDir); cerr != nil {
		return fmt.Errorf("failed to convert upper block to erofs layer: %w", cerr)
	}
	return nil
}

// generate a metadata-only EROFS fsmeta.erofs if all EROFS layer blobs are valid
func (s *snapshotter) generateFsMeta(ctx context.Context, snapIDs []string) {
	var blobs []string

	if s.fsMergeThreshold == 0 || uint(len(snapIDs)) <= s.fsMergeThreshold {
		return
	}

	t1 := time.Now()
	mergedMeta := s.fsMetaPath(snapIDs[0])
	// If the empty placeholder cannot be created (mainly due to os.IsExist), just return
	if _, err := os.OpenFile(mergedMeta, os.O_CREATE|os.O_EXCL, 0644); err != nil {
		return
	}

	for i := len(snapIDs) - 1; i >= 0; i-- {
		blob := s.layerBlobPath(snapIDs[i])
		if _, err := os.Stat(blob); err != nil {
			return
		}
		blobs = append(blobs, blob)
	}
	tmpMergedMeta := mergedMeta + ".tmp"
	args := append([]string{"--aufs", "--ovlfs-strip=1", "--quiet", tmpMergedMeta}, blobs...)
	log.G(ctx).Infof("merging layers with mkfs.erofs %v", args)
	cmd := exec.CommandContext(ctx, "mkfs.erofs", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.G(ctx).Warnf("failed to generate merged fsmeta for %v: %q: %v", snapIDs[0], string(out), err)
		return
	}
	// Atomically replace the fsmeta with the generated file
	if err = os.Rename(tmpMergedMeta, mergedMeta); err != nil {
		log.G(ctx).Errorf("failed to rename fsmeta: %v", err)
		return
	}
	log.G(ctx).WithFields(log.Fields{
		"d": time.Since(t1),
	}).Infof("merged fsmeta for %v generated", snapIDs[0])
}

func (s *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	var layerBlob string
	var id string

	// Apply the overlayfs upperdir (generated by non-EROFS differs) into a EROFS blob
	// in a read transaction first since conversion could be slow.
	err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		sid, _, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}
		id = sid
		return err
	})
	if err != nil {
		return err
	}

	// If the layer blob doesn't exist, which means this layer wasn't applied by
	// the EROFS differ (possibly the walking differ), convert the upperdir instead.
	layerBlob = s.layerBlobPath(id)
	if _, err := os.Stat(layerBlob); err != nil {
		if cerr := s.commitBlock(ctx, layerBlob, id); cerr != nil {
			if errdefs.IsNotImplemented(cerr) {
				return err
			}
			return cerr
		}
	}

	// Enable fsverity on the EROFS layer if configured
	if s.enableFsverity {
		if err := fsverity.Enable(layerBlob); err != nil {
			return fmt.Errorf("failed to enable fsverity: %w", err)
		}
	}

	// Set IMMUTABLE_FL on the EROFS layer to avoid artificial data loss
	if s.setImmutable {
		if err := setImmutable(layerBlob, true); err != nil {
			log.G(ctx).WithError(err).Warnf("failed to set IMMUTABLE_FL for %s", layerBlob)
		}
	}

	return s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		if _, err := os.Stat(layerBlob); err != nil {
			return fmt.Errorf("failed to get the converted erofs blob: %w", err)
		}

		usage, err := fs.DiskUsage(ctx, layerBlob)
		if err != nil {
			return err
		}
		if _, err = storage.CommitActive(ctx, key, name, snapshots.Usage(usage), opts...); err != nil {
			return fmt.Errorf("failed to commit snapshot %s: %w", key, err)
		}
		return nil
	})
}

func (s *snapshotter) Mounts(ctx context.Context, key string) (_ []mount.Mount, err error) {
	var snap storage.Snapshot
	var info snapshots.Info
	if err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		snap, err = storage.GetSnapshot(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to get active mount: %w", err)
		}

		_, info, _, err = storage.GetInfo(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to get snapshot info: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	mounts, err := s.mounts(snap, info)
	if err != nil {
		return nil, err
	}
	return mounts, nil
}

func (s *snapshotter) getCleanupDirectories(ctx context.Context) ([]string, error) {
	ids, err := storage.IDMap(ctx)
	if err != nil {
		return nil, err
	}

	snapshotDir := filepath.Join(s.root, "snapshots")
	fd, err := os.Open(snapshotDir)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	dirs, err := fd.Readdirnames(0)
	if err != nil {
		return nil, err
	}

	cleanup := []string{}
	for _, d := range dirs {
		if _, ok := ids[d]; ok {
			continue
		}
		cleanup = append(cleanup, filepath.Join(snapshotDir, d))
	}

	return cleanup, nil
}

// Remove abandons the snapshot identified by key. The snapshot will
// immediately become unavailable and unrecoverable. Disk space will
// be freed up on the next call to `Cleanup`.
func (s *snapshotter) Remove(ctx context.Context, key string) (err error) {
	var removals []string
	var id string
	// Remove directories after the transaction is closed, failures must not
	// return error since the transaction is committed with the removal
	// key no longer available.
	defer func() {
		if err == nil {
			cleanup := cleanupUpper
			if s.blockMode {
				cleanup = cleanupActiveMounts
			}
			if err := cleanup(s.upperPath(id)); err != nil {
				log.G(ctx).WithError(err).WithField("id", id).Warnf("failed to cleanup upperdir")
			}

			for _, dir := range removals {
				if err := os.RemoveAll(dir); err != nil {
					log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove directory")
				}
			}
		}
	}()
	return s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		var k snapshots.Kind

		id, k, err = storage.Remove(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to remove snapshot %s: %w", key, err)
		}
		// Note: The prepared marker file (if any) is removed when the snapshot
		// directory is cleaned up below.

		removals, err = s.getCleanupDirectories(ctx)
		if err != nil {
			return fmt.Errorf("unable to get directories for removal: %w", err)
		}
		// The layer blob is only persisted for committed snapshots.
		if k == snapshots.KindCommitted {
			// Clear IMMUTABLE_FL before removal, since this flag avoids it.
			err = setImmutable(s.layerBlobPath(id), false)
			if err != nil && !errdefs.IsNotImplemented(err) {
				return fmt.Errorf("failed to clear IMMUTABLE_FL: %w", err)
			}
		}
		return nil
	})
}

func (s *snapshotter) Cleanup(ctx context.Context) (err error) {
	var removals []string
	if err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		var err error
		removals, err = s.getCleanupDirectories(ctx)
		return err
	}); err != nil {
		return err
	}

	cleanup := cleanupUpper
	if s.blockMode {
		cleanup = cleanupActiveMounts
	}

	for _, dir := range removals {
		_ = cleanup(filepath.Join(dir, "fs"))
		_ = setImmutable(filepath.Join(dir, "layer.erofs"), false)
		if err := os.RemoveAll(dir); err != nil {
			log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove directory")
		}
	}
	return nil
}

func (s *snapshotter) Stat(ctx context.Context, key string) (info snapshots.Info, err error) {
	err = s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		_, info, _, err = storage.GetInfo(ctx, key)
		return err
	})
	if err != nil {
		return snapshots.Info{}, err
	}

	return info, nil
}

func (s *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (_ snapshots.Info, err error) {
	err = s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		info, err = storage.UpdateInfo(ctx, info, fieldpaths...)
		return err
	})
	if err != nil {
		return snapshots.Info{}, err
	}

	return info, nil
}

func (s *snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, fs ...string) error {
	return s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		return storage.WalkInfo(ctx, fn, fs...)
	})
}

// Usage returns the resources taken by the snapshot identified by key.
//
// For active snapshots, this will scan the usage of the overlay "diff" (aka
// "upper") directory and may take some time.
//
// For committed snapshots, the value is returned from the metadata database.
func (s *snapshotter) Usage(ctx context.Context, key string) (_ snapshots.Usage, err error) {
	var (
		usage snapshots.Usage
		info  snapshots.Info
		id    string
	)
	if err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		id, info, usage, err = storage.GetInfo(ctx, key)
		return err
	}); err != nil {
		return usage, err
	}

	if info.Kind == snapshots.KindActive {
		upperPath := s.upperPath(id)
		du, err := fs.DiskUsage(ctx, upperPath)
		if err != nil {
			// TODO(stevvooe): Consider not reporting an error in this case.
			return snapshots.Usage{}, err
		}
		usage = snapshots.Usage(du)
	}
	return usage, nil
}

// Add a method to verify fsverity
func (s *snapshotter) verifyFsverity(path string) error {
	if !s.enableFsverity {
		return nil
	}
	enabled, err := fsverity.IsEnabled(path)
	if err != nil {
		return fmt.Errorf("failed to check fsverity status: %w", err)
	}
	if !enabled {
		return fmt.Errorf("fsverity is not enabled on %s", path)
	}
	return nil
}
