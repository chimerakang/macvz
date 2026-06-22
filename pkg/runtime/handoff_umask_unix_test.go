//go:build !windows

package runtime

import (
	"os"
	"syscall"
	"testing"
)

// syscallUmask sets the process umask and returns the previous value, so a test
// can prove Create forces the handoff mode regardless of the inherited umask.
func syscallUmask(mask int) int { return syscall.Umask(mask) }

// statFileOwner returns the uid/gid that owns path so a test can prove the
// handoff directory was chowned to the configured container user. ok is false
// when the platform does not expose unix ownership.
func statFileOwner(t *testing.T, path string) (uid, gid int, ok bool) {
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
