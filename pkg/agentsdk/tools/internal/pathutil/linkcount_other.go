//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package pathutil

import (
	"fmt"
	"os"
)

func RequireSingleLink(os.FileInfo) error {
	return fmt.Errorf("secure hardlink validation is unsupported on this platform")
}
