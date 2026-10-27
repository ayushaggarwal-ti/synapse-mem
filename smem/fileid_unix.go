//go:build unix

package smem

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity identifies the database file itself (device and inode), so a
// copy can be told apart from the original even at the same path.
func fileIdentity(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}
