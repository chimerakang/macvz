//go:build !windows

package runtime

import "syscall"

// syscallUmask sets the process umask and returns the previous value, so a test
// can prove Create forces the handoff mode regardless of the inherited umask.
func syscallUmask(mask int) int { return syscall.Umask(mask) }
