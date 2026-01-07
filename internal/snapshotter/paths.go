package snapshotter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/v2/core/snapshots"
)

const (
	// fallbackLayerPrefix is used for layers created by the walking differ fallback
	// when the original layer digest is not available.
	fallbackLayerPrefix = "snapshot-"
)

// Snapshot directory structure constants.
const (
	// snapshotsDirName is the name of the directory containing all snapshots.
	snapshotsDirName = "snapshots"

	// fsDirName is the overlay upper directory name within a snapshot.
	fsDirName = "fs"

	// rwLayerFilename is the filename for the ext4 writable layer image.
	rwLayerFilename = "rwlayer.img"

	// rwDirName is the directory name for the mounted ext4 rw layer.
	rwDirName = "rw"

	// upperDirName is the overlay upper directory name within the rw mount.
	upperDirName = "upper"

	// lowerDirName is the directory name for view snapshot lower paths.
	lowerDirName = "lower"

	// fsmetaFilename is the filename for merged fsmeta EROFS.
	fsmetaFilename = "fsmeta.erofs"

	// vmdkFilename is the filename for the VMDK descriptor.
	vmdkFilename = "merged.vmdk"

	// layersManifestFilename is the filename for the layer order manifest.
	layersManifestFilename = "layers.manifest"
)

// upperPath returns the path to the overlay upper directory for a snapshot.
func (s *snapshotter) upperPath(id string) string {
	return filepath.Join(s.root, snapshotsDirName, id, fsDirName)
}

// writablePath returns the path to the ext4 writable layer image file.
func (s *snapshotter) writablePath(id string) string {
	return filepath.Join(s.root, snapshotsDirName, id, rwLayerFilename)
}

// blockRwMountPath returns the mount point for the ext4 rwlayer in block mode.
func (s *snapshotter) blockRwMountPath(id string) string {
	return filepath.Join(s.root, snapshotsDirName, id, rwDirName)
}

// blockUpperPath returns the overlay upperdir inside the mounted ext4.
func (s *snapshotter) blockUpperPath(id string) string {
	return filepath.Join(s.blockRwMountPath(id), upperDirName)
}

// findLayerBlob finds the EROFS layer blob for a snapshot.
// First checks the LabelLayerBlobPath label, then falls back to globbing
// for *.erofs files in the snapshot directory.
func (s *snapshotter) findLayerBlob(ctx context.Context, id string) (string, error) {
	info, err := s.getSnapshotInfoByID(ctx, id)
	if err != nil {
		// Info lookup failed, but we can still try the glob fallback
		info = snapshots.Info{}
	}

	return s.findLayerBlobFromInfo(id, info)
}

// findLayerBlobFromInfo finds the EROFS layer blob using the snapshot's labels.
// Falls back to globbing for *.erofs files in the snapshot directory if the
// label isn't set (for compatibility with EROFS differ which creates blobs
// directly without setting labels).
func (s *snapshotter) findLayerBlobFromInfo(id string, info snapshots.Info) (string, error) {
	snapshotDir := filepath.Join(s.root, snapshotsDirName, id)
	var searched []string

	// First check the label (preferred)
	if blobPath := info.Labels[LabelLayerBlobPath]; blobPath != "" {
		if _, err := os.Stat(blobPath); err == nil {
			return blobPath, nil
		}
		searched = append(searched, blobPath+" (from label, file missing)")
	}

	// Fallback: glob for any *.erofs file in the snapshot directory
	// This handles cases where the EROFS differ created the blob directly
	pattern := filepath.Join(snapshotDir, "*.erofs")
	matches, _ := filepath.Glob(pattern)
	for _, match := range matches {
		// Skip fsmeta files
		if filepath.Base(match) == fsmetaFilename {
			continue
		}
		return match, nil
	}
	searched = append(searched, pattern+" (glob found nothing)")

	return "", &LayerBlobNotFoundError{
		SnapshotID: id,
		Dir:        snapshotDir,
		Searched:   searched,
	}
}

// fallbackLayerBlobPath returns the path for creating a layer blob when the
// digest is not available (walking differ fallback). Uses the snapshot ID.
func (s *snapshotter) fallbackLayerBlobPath(id string) string {
	return filepath.Join(s.root, snapshotsDirName, id, fallbackLayerPrefix+id+".erofs")
}

// fsMetaPath returns the path to the merged fsmeta.erofs file.
func (s *snapshotter) fsMetaPath(id string) string {
	return filepath.Join(s.root, snapshotsDirName, id, fsmetaFilename)
}

// vmdkPath returns the path to the VMDK descriptor file.
func (s *snapshotter) vmdkPath(id string) string {
	return filepath.Join(s.root, snapshotsDirName, id, vmdkFilename)
}

// layersManifestPath returns the path to the layers.manifest file.
func (s *snapshotter) layersManifestPath(id string) string {
	return filepath.Join(s.root, snapshotsDirName, id, layersManifestFilename)
}

// viewLowerPath returns the path to the lower directory for View snapshots.
func (s *snapshotter) viewLowerPath(id string) string {
	return filepath.Join(s.root, snapshotsDirName, id, lowerDirName)
}

// snapshotDir returns the path to a snapshot directory.
func (s *snapshotter) snapshotDir(id string) string {
	return filepath.Join(s.root, snapshotsDirName, id)
}

// snapshotsDir returns the path to the snapshots root directory.
func (s *snapshotter) snapshotsDir() string {
	return filepath.Join(s.root, snapshotsDirName)
}

// lowerPath returns the EROFS layer blob path for a snapshot, validating it exists.
func (s *snapshotter) lowerPath(ctx context.Context, id string) (string, error) {
	layerBlob, err := s.findLayerBlob(ctx, id)
	if err != nil {
		return "", fmt.Errorf("failed to find valid erofs layer blob: %w", err)
	}

	return layerBlob, nil
}
