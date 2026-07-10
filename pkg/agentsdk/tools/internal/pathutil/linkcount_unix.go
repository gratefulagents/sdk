//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package pathutil

import (
	"fmt"
	"os"
	"syscall"
)

// RequireSingleLink rejects regular files with more than one directory entry.
// A path can be lexically inside the workspace while its inode is also linked
// outside it; refusing multiply-linked inputs prevents that alias from crossing
// the workspace read boundary.
func RequireSingleLink(info os.FileInfo) error {
	if info == nil {
		return fmt.Errorf("file metadata is required")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("file link count is unavailable")
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("file must have exactly one hard link")
	}
	return nil
}
