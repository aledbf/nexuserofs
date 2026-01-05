# Udev Loop Device Optimization

## Problem

When nexuserofs mounts container images, it creates loop devices for each EROFS filesystem layer. A typical container image with 5 layers results in 5 loop devices.

This triggers CPU spikes because udev workers attempt SCSI ID detection on each loop device, spawning multiple processes per device.

### Symptoms

```
Jan 05 00:40:05 mini-pc kernel: loop0: detected capacity change from 0 to 48
Jan 05 00:40:05 mini-pc kernel: erofs (device loop0): mounted with root inode @ nid 36.
Jan 05 00:40:05 mini-pc 55-scsi-sg3_id.rules[1866808]: WARNING: SCSI device loop2 has no device ID...
Jan 05 00:40:05 mini-pc 55-scsi-sg3_id.rules[1866806]: WARNING: SCSI device loop0 has no device ID...
Jan 05 00:40:05 mini-pc 55-scsi-sg3_id.rules[1866807]: WARNING: SCSI device loop1 has no device ID...
```

The warnings repeat multiple times per device as udev retries the detection.

## Root Cause

The `55-scsi-sg3_id.rules` udev rule (from the `sg3-utils` package) runs SCSI ID detection on block devices to populate device identification attributes. This is useful for real SCSI/SAS/SATA devices but pointless for loop devices which:

1. Don't have SCSI device IDs
2. Are virtual devices backed by files
3. Are created/destroyed frequently during container operations

The detection process:
1. Loop device creation triggers udev `add` event
2. `55-scsi-sg3_id.rules` matches the block device
3. Multiple `sg_inq` processes spawn to query SCSI attributes
4. All queries fail (loop devices aren't SCSI)
5. CPU cycles wasted, warnings logged

## Solution

Skip SCSI ID detection and suppress warnings for all loop devices. Loop devices are **never** SCSI devices, so this is always safe.

### Installation

Create a udev rule file numbered **before** `55-scsi-sg3_id.rules` (e.g., `49-`):

```bash
sudo tee /etc/udev/rules.d/49-skip-loop-scsi.rules << 'EOF'
# Skip SCSI detection and warnings for all loop devices
# Sets ID_SERIAL to prevent the 55-scsi-sg3_id.rules warning
SUBSYSTEM=="block", KERNEL=="loop*", ENV{ID_SERIAL}="loop", ENV{ID_SCSI}="0", ENV{ID_SCSI_INQUIRY}="0"

# Reduce CPU overhead from udisksd and systemd for loop devices
SUBSYSTEM=="block", KERNEL=="loop*", ENV{UDISKS_IGNORE}="1"
SUBSYSTEM=="block", KERNEL=="loop*", ENV{SYSTEMD_READY}="0", OPTIONS+="nowatch"
EOF

sudo udevadm control --reload-rules
```

### How It Works

| Rule Component | Purpose |
|----------------|---------|
| `SUBSYSTEM=="block"` | Match block devices only |
| `KERNEL=="loop*"` | Match loop device names (loop0, loop1, etc.) |
| `ENV{ID_SERIAL}="loop"` | Set a serial to prevent "no device ID" warning |
| `ENV{ID_SCSI}="0"` | Mark device as non-SCSI |
| `ENV{ID_SCSI_INQUIRY}="0"` | Disable SCSI inquiry commands |
| `ENV{UDISKS_IGNORE}="1"` | Tell udisksd to ignore the device (reduces CPU) |
| `ENV{SYSTEMD_READY}="0"` | Skip systemd device unit creation |
| `OPTIONS+="nowatch"` | Disable inotify watching |

The rule file is numbered `49-` to ensure it runs **before** `55-scsi-sg3_id.rules`.

The `55-scsi-sg3_id.rules` file logs a warning for any disk device without `ID_SERIAL`:
```
ENV{ID_SERIAL}!="?*", ENV{DEVTYPE}=="disk", PROGRAM="/bin/logger ... WARNING: SCSI device %k has no device ID ..."
```

By setting `ID_SERIAL="loop"`, this warning condition becomes false and no warning is logged.

### Verification

After applying the rules:

```bash
# Reload rules
sudo udevadm control --reload-rules

# Trigger a change event on existing loop devices
sudo udevadm trigger --subsystem-match=block --attr-match=loop/backing_file

# Verify properties are set correctly
udevadm info --query=property --name=/dev/loop0 | grep -E 'ID_SERIAL|ID_SCSI'

# Monitor udev events during container operations
udevadm monitor --property --subsystem-match=block
```

Expected output:
```
ID_SERIAL=loop
ID_SCSI=0
ID_SCSI_INQUIRY=0
```

## Serial Numbers for Device Identification

nexuserofs still sets serial numbers on loop devices for **identification purposes** (not for udev matching):

- Format: `erofs-<snapshot-id>` (e.g., `erofs-42`, `erofs-sha256-abc123`)
- Written to `/sys/block/loopN/loop/serial` (requires kernel 5.17+)
- Used by nexuserofs to find/manage its loop devices
- Visible via: `cat /sys/block/loop0/loop/serial`

### Why Not Match on Serial?

You might wonder why we don't match on `ENV{ID_SERIAL}=="erofs-*"` instead of all loop devices. The reason is a **race condition**:

1. Loop device is created → udev `add` event fires immediately
2. nexuserofs writes serial to sysfs AFTER device creation
3. udev rules process the event BEFORE serial is written
4. `55-scsi-sg3_id.rules` runs, serial isn't set yet, warnings appear

Since loop devices are never SCSI devices, skipping SCSI detection for ALL loop devices is the correct, race-free solution.

## Systemd Mount Unit Overhead

By default, systemd creates a `.mount` unit for every mount point it sees. With many EROFS layers, this causes:
- High CPU usage from systemd's mount unit generator
- Log spam with "Deactivated successfully" messages
- Thousands of transient mount units

### Solution: Private Mount Namespace

The nexuserofs systemd service uses `MountFlags=private` to run in a private mount namespace. This hides all EROFS mounts from systemd's mount unit generator.

```ini
[Service]
# Prevents systemd from creating mount units for EROFS layers
MountFlags=private
```

For tests, run in a private mount namespace:

```bash
sudo unshare -m sh -c 'mount --make-rprivate / && go test -race -cover ./... -test.root'
```

## Performance Impact

Without the fixes:
- 5-10 `sg_inq` processes per loop device
- Multiple retry attempts from SCSI detection
- High CPU from systemd mount unit generation
- High CPU from udisksd monitoring
- Log spam with warnings

With the fixes:
- Zero `sg_inq` processes for loop devices
- No systemd mount units for EROFS layers
- No udisksd monitoring overhead
- Clean logs
- Fast container startup

## Requirements

- Linux kernel 5.17+ (for loop device serial number support via sysfs)
- `sg3-utils` package installed (the source of SCSI detection overhead)

## References

- [udev(7) man page](https://man7.org/linux/man-pages/man7/udev.7.html)
- [sg3_utils documentation](https://sg.danny.cz/sg/sg3_utils.html)
- [Linux loop device documentation](https://man7.org/linux/man-pages/man4/loop.4.html)
- [Kernel commit: loop: add serial number sysfs attribute](https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/commit/?id=fc755d1e0c1e)
