package snapshotter

import (
	"encoding/json"

	"github.com/opencontainers/go-digest"
)

// Label keys for nexus-erofs-snapshotter metadata.
// These are stored in containerd's snapshot labels and survive restarts.
//
// Labels provide several advantages over filesystem-based metadata:
// - Atomic updates within snapshot transactions
// - Queryable via containerd API (ctr snapshots ls --filter)
// - No filesystem stat() calls for readiness checks
// - Survives filesystem corruption better than marker files
const (
	// LabelPrefix is the namespace for all nexus-erofs-snapshotter labels.
	LabelPrefix = "nexus-erofs-snapshotter/"

	// LabelValueTrue is the standard value for boolean labels set to true.
	LabelValueTrue = "true"

	// LabelExtract marks a snapshot for layer extraction by the differ.
	// When set to "true", mounts() returns diff mounts (bind to upper directory)
	// instead of the normal EROFS/ext4 mounts.
	//
	// Value: "true" or absent
	// Set during: Prepare (for extract/unpack keys)
	// Read by: isExtractSnapshot() in mounts()
	LabelExtract = LabelPrefix + "extract"

	// LabelLayerDigest stores the OCI digest of the committed EROFS layer.
	// This enables direct path construction without glob patterns.
	//
	// Value: "sha256:abc123..." (digest.Digest string format)
	// Set during: Commit (extracted from layer blob path or differ)
	// Read by: findLayerBlob() for fast path lookup
	LabelLayerDigest = LabelPrefix + "layer-digest"

	// LabelLayerBlobPath stores the absolute path to the EROFS layer blob.
	// Avoids expensive glob operations to discover layer files.
	//
	// Value: "/var/lib/nexus-erofs-snapshotter/snapshots/123/sha256-abc.erofs"
	// Set during: Commit
	// Read by: findLayerBlob(), lowerPath()
	LabelLayerBlobPath = LabelPrefix + "layer-blob-path"

	// LabelFsmetaReady indicates fsmeta and VMDK generation completed successfully.
	// When set, mountFsMeta() can skip filesystem existence checks.
	//
	// Value: "true" or absent
	// Set during: generateFsMeta() on successful completion
	// Read by: mountFsMeta() for fast path
	LabelFsmetaReady = LabelPrefix + "fsmeta-ready"

	// LabelLayerOrder stores layer digests in VMDK/OCI order (oldest-first).
	// This replaces the layers.manifest file and enables external verification.
	//
	// Value: JSON array ["sha256:oldest...", "sha256:newest..."]
	// Set during: generateFsMeta() on success
	// Read by: External tools, verification utilities
	LabelLayerOrder = LabelPrefix + "layer-order"

	// LabelMountType hints at the expected mount type for this snapshot.
	// Allows quick mount type determination without filesystem inspection.
	//
	// Value: "format/erofs" | "erofs" | "ext4" | "bind"
	// Set during: Commit or first View creation
	// Read by: mounts() for optimization (optional)
	LabelMountType = LabelPrefix + "mount-type"

	// LabelWritableSize stores the size of the ext4 writable layer in bytes.
	// Informational label for debugging and capacity planning.
	//
	// Value: "67108864" (string representation of int64)
	// Set during: Prepare (for active snapshots)
	// Read by: Informational only
	LabelWritableSize = LabelPrefix + "writable-size"

	// LabelConversionError stores the last EROFS conversion error message.
	// Useful for debugging failed commits without searching logs.
	//
	// Value: Human-readable error string (truncated to 256 chars)
	// Set during: Commit on conversion failure
	// Read by: Debugging, ctr snapshots info
	LabelConversionError = LabelPrefix + "conversion-error"

	// LabelFsmetaLayers stores the count of layers in the fsmeta.
	// Quick access to layer count without parsing LabelLayerOrder JSON.
	//
	// Value: "5" (string representation of int)
	// Set during: generateFsMeta()
	// Read by: Quick layer count checks
	LabelFsmetaLayers = LabelPrefix + "fsmeta-layers"

	// LabelImmutable indicates the layer blob has IMMUTABLE_FL attribute set.
	// Tracked so Remove() knows whether to clear the flag before deletion.
	//
	// Value: "true" or absent
	// Set during: Commit (when setImmutable config is enabled)
	// Read by: Remove() to clear flag before deletion
	LabelImmutable = LabelPrefix + "immutable"
)

// Mount type constants for LabelMountType values.
const (
	MountTypeFormatErofs = "format/erofs" // Multi-layer fsmeta (VM-only)
	MountTypeErofs       = "erofs"        // Single EROFS layer
	MountTypeExt4        = "ext4"         // Writable ext4 layer
	MountTypeBind        = "bind"         // Bind mount (extract snapshots)
)

// EncodeLayerOrder encodes a slice of digests to JSON for LabelLayerOrder.
func EncodeLayerOrder(digests []digest.Digest) string {
	if len(digests) == 0 {
		return "[]"
	}
	strs := make([]string, len(digests))
	for i, d := range digests {
		strs[i] = d.String()
	}
	data, err := json.Marshal(strs)
	if err != nil {
		return "[]"
	}
	return string(data)
}

// DecodeLayerOrder decodes LabelLayerOrder JSON back to a slice of digests.
// Returns nil on empty or invalid input.
func DecodeLayerOrder(encoded string) []digest.Digest {
	if encoded == "" || encoded == "[]" {
		return nil
	}
	var strs []string
	if err := json.Unmarshal([]byte(encoded), &strs); err != nil {
		return nil
	}
	digests := make([]digest.Digest, 0, len(strs))
	for _, s := range strs {
		d, err := digest.Parse(s)
		if err != nil {
			continue // Skip invalid digests
		}
		digests = append(digests, d)
	}
	return digests
}
