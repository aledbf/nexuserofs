# nexuserofs

External snapshotter plugin for containerd that leverages EROFS (Enhanced Read-Only File System) for container image layers.

## Overview

An external EROFS snapshotter that communicates with containerd via gRPC socket. This allows using EROFS-based container layers without modifying containerd.

## Architecture

```
┌─────────────────────┐     gRPC/socket     ┌────────────────────────┐
│     containerd      │◄───────────────────►│  erofs-snapshotter     │
│                     │                     │  (this project)        │
│  - content store    │                     │                        │
│  - images           │◄────client conn─────│  Implements:           │
│  - proxy plugins    │    (for content)    │  - SnapshotsServer     │
└─────────────────────┘                     │  - DiffServer          │
                                            └────────────────────────┘
```

## Requirements

- Linux kernel with EROFS support (5.4+)
- erofs-utils (mkfs.erofs)
- containerd 2.0+

## Building

```bash
make build
```

## Running

```bash
# Start the snapshotter daemon
sudo ./bin/erofs-snapshotter --config /etc/erofs-snapshotter/config.toml

# Or with flags
sudo ./bin/erofs-snapshotter \
  --root /var/lib/erofs-snapshotter \
  --address /run/erofs-snapshotter/snapshotter.sock \
  --containerd-address /run/containerd/containerd.sock
```

## Configuration

### containerd

```toml
# /etc/containerd/config.toml
version = 2

[proxy_plugins]
  [proxy_plugins.erofs]
    type = "snapshot"
    address = "/run/erofs-snapshotter/snapshotter.sock"

  [proxy_plugins.erofs-diff]
    type = "diff"
    address = "/run/erofs-snapshotter/snapshotter.sock"

# Use as default snapshotter
[plugins."io.containerd.cri.v1.images"]
  snapshotter = "erofs"
```

### Snapshotter

```toml
# /etc/erofs-snapshotter/config.toml
version = 1

[snapshotter]
root = "/var/lib/erofs-snapshotter"
address = "/run/erofs-snapshotter/snapshotter.sock"

# Connect to containerd for content store access
containerd_address = "/run/containerd/containerd.sock"
containerd_namespace = "default"

[snapshotter.options]
# Default writable layer size (0 = directory mode, >0 = block mode)
# Block mode uses ext4 loop mounts for writable layers
default_writable_size = "512M"

# Enable fsverity for layer integrity validation
enable_fsverity = false

# Set immutable flag on committed layers
set_immutable = true

# Extra overlay mount options
overlay_options = ["index=off", "metacopy=off"]

# Max layers before triggering fsmerge (0 = disabled)
max_unmerged_layers = 0

[differ]
# Extra mkfs.erofs options
mkfs_options = ["-zlz4hc,12", "-C65536"]

# Enable tar index mode (faster apply, requires erofs-utils 1.8+)
tar_index_mode = false
```

## Project Structure

```
nexuserofs/
├── cmd/erofs-snapshotter/    # Entry point, gRPC server setup
├── pkg/
│   ├── snapshotter/          # Core snapshotter implementation
│   ├── differ/               # EROFS differ implementation
│   └── erofs/                # mkfs.erofs wrapper, mount handling
├── internal/
│   ├── fsverity/             # fsverity support
│   ├── mountutils/           # Mount utilities
│   └── cleanup/              # Cleanup utilities
└── config/                   # Example configuration files
```

## License

Apache 2.0
