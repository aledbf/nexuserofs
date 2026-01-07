# Nexus EROFS Snapshotter Annotations

This document describes the labels/annotations used by the nexus-erofs-snapshotter to store metadata in containerd's snapshot labels.

## Overview

The snapshotter uses containerd snapshot labels (stored in BoltDB via MetaStore) as the primary source of metadata. This approach:

- Eliminates filesystem stat calls for readiness checks
- Removes the need for manifest files
- Enables querying via containerd API (`ctr snapshots ls --filter`)
- Ensures atomicity with snapshot operations (labels survive crashes)

All labels use the prefix `nexus-erofs-snapshotter/`.

## Label Reference

### `nexus-erofs-snapshotter/extract`

Marks a snapshot for layer extraction by the differ.

| Property | Value |
|----------|-------|
| Value | `"true"` or absent |
| Set during | `Prepare()` for extract keys |
| Used by | `mounts()` to return diff mounts |

**Example:**
```bash
ctr snapshots ls --filter 'labels."nexus-erofs-snapshotter/extract"==true'
```

### `nexus-erofs-snapshotter/layer-digest`

Stores the OCI digest of the committed layer.

| Property | Value |
|----------|-------|
| Value | `"sha256:abc123..."` (digest.Digest string) |
| Set during | `Commit()` |
| Used by | External tools, debugging |

### `nexus-erofs-snapshotter/layer-blob-path`

Stores the absolute path to the EROFS layer blob.

| Property | Value |
|----------|-------|
| Value | `/var/lib/nexus-erofs-snapshotter/snapshots/123/sha256-abc.erofs` |
| Set during | `Commit()` |
| Used by | `findLayerBlob()` to locate layer without filesystem scanning |

**Note:** This is the authoritative source for layer blob location. The snapshotter no longer uses glob patterns to discover blobs.

### `nexus-erofs-snapshotter/fsmeta-ready`

Indicates fsmeta and VMDK generation completed successfully.

| Property | Value |
|----------|-------|
| Value | `"true"` or absent |
| Set during | `generateFsMeta()` on success |
| Used by | `mountFsMeta()` to determine if fsmeta mount is available |

**Example:**
```bash
ctr snapshots ls --filter 'labels."nexus-erofs-snapshotter/fsmeta-ready"==true'
```

### `nexus-erofs-snapshotter/fsmeta-layers`

Stores the count of layers in the fsmeta.

| Property | Value |
|----------|-------|
| Value | `"5"` (string representation of int) |
| Set during | `generateFsMeta()` |
| Used by | Quick layer count without parsing layer-order |

### `nexus-erofs-snapshotter/layer-order`

Stores the layer digests in VMDK/OCI order (oldest-first).

| Property | Value |
|----------|-------|
| Value | JSON array: `["sha256:oldest...", "sha256:middle...", "sha256:newest..."]` |
| Set during | `generateFsMeta()` on success |
| Used by | External tools for verification |

**Note:** This replaces the previous `layers.manifest` file.

### `nexus-erofs-snapshotter/mount-type`

Hints at the mount type for this snapshot.

| Property | Value |
|----------|-------|
| Value | `"format/erofs"` \| `"erofs"` \| `"ext4"` \| `"bind"` |
| Set during | (Reserved for future use) |
| Used by | Quick decision without filesystem inspection |

### `nexus-erofs-snapshotter/writable-size`

Stores the size of the ext4 writable layer in bytes.

| Property | Value |
|----------|-------|
| Value | `"67108864"` (string representation of int64) |
| Set during | `Prepare()` for active snapshots |
| Used by | Informational, debugging |

### `nexus-erofs-snapshotter/conversion-error`

Stores the last EROFS conversion error (if any).

| Property | Value |
|----------|-------|
| Value | Human-readable error string |
| Set during | `Commit()` on conversion failure |
| Used by | Debugging via `ctr snapshots info` |

### `nexus-erofs-snapshotter/immutable`

Indicates the layer blob has IMMUTABLE_FL set (Linux only).

| Property | Value |
|----------|-------|
| Value | `"true"` or absent |
| Set during | `Commit()` when setImmutable is enabled |
| Used by | `Remove()` to know if flag needs clearing |

## Label Lifecycle

### Prepare Phase

When creating a new active snapshot:
- `nexus-erofs-snapshotter/extract` - Set to `"true"` for extract snapshots
- `nexus-erofs-snapshotter/writable-size` - Set to the configured writable layer size

### Commit Phase

When committing a snapshot:
- `nexus-erofs-snapshotter/layer-blob-path` - Absolute path to the EROFS blob
- `nexus-erofs-snapshotter/layer-digest` - OCI digest if available
- `nexus-erofs-snapshotter/immutable` - Set if immutable flag was applied

### FsMeta Generation

When generating fsmeta/VMDK:
- `nexus-erofs-snapshotter/fsmeta-ready` - Set to `"true"` on success
- `nexus-erofs-snapshotter/fsmeta-layers` - Count of layers in the fsmeta
- `nexus-erofs-snapshotter/layer-order` - JSON array of layer digests

## Querying Labels

### Using containerd CLI

```bash
# Find all extract snapshots
ctr snapshots ls --filter 'labels."nexus-erofs-snapshotter/extract"==true'

# Find snapshots with fsmeta ready
ctr snapshots ls --filter 'labels."nexus-erofs-snapshotter/fsmeta-ready"==true'

# Get all labels for a snapshot
ctr snapshots info mycontainer
```

### Using containerd Go API

```go
import "github.com/containerd/containerd/v2/core/snapshots"

// Get snapshot info with labels
info, err := snapshotter.Stat(ctx, "mycontainer")
if err != nil {
    return err
}

// Check if fsmeta is ready
if info.Labels["nexus-erofs-snapshotter/fsmeta-ready"] == "true" {
    // Use fsmeta mount
}

// Get layer blob path
blobPath := info.Labels["nexus-erofs-snapshotter/layer-blob-path"]
```

## Go Constants

All labels are defined in `internal/snapshotter/labels.go`:

```go
const (
    LabelPrefix          = "nexus-erofs-snapshotter/"
    LabelExtract         = LabelPrefix + "extract"
    LabelLayerDigest     = LabelPrefix + "layer-digest"
    LabelLayerBlobPath   = LabelPrefix + "layer-blob-path"
    LabelFsmetaReady     = LabelPrefix + "fsmeta-ready"
    LabelLayerOrder      = LabelPrefix + "layer-order"
    LabelMountType       = LabelPrefix + "mount-type"
    LabelWritableSize    = LabelPrefix + "writable-size"
    LabelConversionError = LabelPrefix + "conversion-error"
    LabelFsmetaLayers    = LabelPrefix + "fsmeta-layers"
    LabelImmutable       = LabelPrefix + "immutable"
)
```

## Helper Functions

### EncodeLayerOrder / DecodeLayerOrder

For encoding/decoding the `layer-order` JSON array:

```go
import "github.com/aledbf/nexus-erofs/internal/snapshotter"

// Encode digests to JSON
digests := []digest.Digest{
    "sha256:abc123...",
    "sha256:def456...",
}
encoded := snapshotter.EncodeLayerOrder(digests) // Returns JSON string

// Decode JSON to digests
decoded, err := snapshotter.DecodeLayerOrder(encoded)
```
