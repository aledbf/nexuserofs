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
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/containerd/continuity/fs"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"golang.org/x/sys/unix"

	"github.com/containerd/containerd/v2/core/mount"
	erofsutils "github.com/aledbf/nexuserofs/pkg/erofs"
)

// defaultWritableSize is set to 0 for Linux to match the default behavior of
// other snapshotters available on Linux.
const defaultWritableSize = 0

// check if EROFS kernel filesystem is registered or not
func findErofs() bool {
	fs, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return false
	}
	return bytes.Contains(fs, []byte("\terofs\n"))
}

func checkCompatibility(root string) error {
	supportsDType, err := fs.SupportsDType(root)
	if err != nil {
		return err
	}
	if !supportsDType {
		return fmt.Errorf("%s does not support d_type. If the backing filesystem is xfs, please reformat with ftype=1 to enable d_type support", root)
	}

	if !findErofs() {
		return fmt.Errorf("EROFS unsupported, please `modprobe erofs`: %w", plugin.ErrSkipPlugin)
	}

	return nil
}

func setImmutable(path string, enable bool) error {
	//nolint:revive,staticcheck	// silence "don't use ALL_CAPS in Go names; use CamelCase"
	const (
		FS_IMMUTABLE_FL = 0x10
	)
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open: %w", err)
	}
	defer f.Close()

	oldattr, err := unix.IoctlGetInt(int(f.Fd()), unix.FS_IOC_GETFLAGS)
	if err != nil {
		return fmt.Errorf("error getting inode flags: %w", err)
	}
	newattr := oldattr | FS_IMMUTABLE_FL
	if !enable {
		newattr ^= FS_IMMUTABLE_FL
	}
	if newattr == oldattr {
		return nil
	}
	return unix.IoctlSetPointerInt(int(f.Fd()), unix.FS_IOC_SETFLAGS, newattr)
}

func cleanupUpper(upper string) error {
	return unmountAll(upper)
}

// cleanupActiveMounts unmounts all active mounts under the upper directory.
// This is a best-effort cleanup that continues to clean up remaining mounts
// even if individual unmounts fail. Errors are logged and the last error
// encountered is returned.
func cleanupActiveMounts(upper string) error {
	var lastErr error

	merged := filepath.Join(upper, "merged")
	lower := filepath.Join(upper, "lower")
	rw := filepath.Join(upper, "rw")

	if err := unmountAll(merged); err != nil {
		log.L.WithError(err).Warnf("failed to unmount merged directory %s during cleanup", merged)
		lastErr = err
	}

	if entries, err := os.ReadDir(lower); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			target := filepath.Join(lower, e.Name())
			if err := unmountAll(target); err != nil {
				log.L.WithError(err).Warnf("failed to unmount lower directory %s during cleanup", target)
				lastErr = err
			}
		}
	}

	if err := unmountAll(rw); err != nil {
		log.L.WithError(err).Warnf("failed to unmount rw directory %s during cleanup", rw)
		lastErr = err
	}

	return lastErr
}

// unmountAll attempts to unmount the target. If normal unmount fails (e.g., due
// to EBUSY), it falls back to lazy unmount (MNT_DETACH) which detaches the mount
// immediately but may leave the mount lingering until all references are closed.
//
// Returns the original error even if lazy unmount succeeds, to make the issue
// visible. Callers should log this but can continue cleanup since the mount
// point is at least detached.
func unmountAll(target string) error {
	if err := mount.UnmountAll(target, 0); err != nil {
		// Normal unmount failed, try lazy unmount as fallback.
		// This detaches the mount immediately but resources may linger.
		if derr := mount.UnmountAll(target, unix.MNT_DETACH); derr != nil {
			// Both normal and lazy unmount failed
			return err
		}
		// Lazy unmount succeeded but something was holding the mount open.
		// Log this as it may indicate a resource leak or cleanup ordering issue.
		log.L.WithError(err).Warnf("unmount of %s required MNT_DETACH fallback, mount may linger", target)
		return err
	}
	return nil
}

func convertDirToErofs(ctx context.Context, layerBlob, upperDir string) error {
	err := erofsutils.ConvertErofs(ctx, layerBlob, upperDir, nil)
	if err != nil {
		return err
	}

	// Remove all sub-directories in the overlayfs upperdir.  Leave the
	// overlayfs upperdir itself since it's used for Lchown.
	fd, err := os.Open(upperDir)
	if err != nil {
		return err
	}
	defer fd.Close()

	dirs, err := fd.Readdirnames(0)
	if err != nil {
		return err
	}

	for _, d := range dirs {
		dir := filepath.Join(upperDir, d)
		if err := os.RemoveAll(dir); err != nil {
			log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove directory")
		}
	}
	return nil
}

func upperDirectoryPermission(p, parent string) error {
	st, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("failed to stat parent: %w", err)
	}

	stat := st.Sys().(*syscall.Stat_t)
	if err := os.Lchown(p, int(stat.Uid), int(stat.Gid)); err != nil {
		return fmt.Errorf("failed to chown: %w", err)
	}

	return nil
}
