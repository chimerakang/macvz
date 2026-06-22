//go:build windows

package runtime

// syscallUmask is a no-op on platforms without a umask; the mode test is skipped
// there.
func syscallUmask(mask int) int { return 0 }
