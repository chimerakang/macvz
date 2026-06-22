//go:build windows

package runtime

import "testing"

// syscallUmask is a no-op on platforms without a umask; the mode test is skipped
// there.
func syscallUmask(mask int) int { return 0 }

// statFileOwner reports no unix ownership on platforms without it; ownership
// assertions are skipped there.
func statFileOwner(_ *testing.T, _ string) (uid, gid int, ok bool) { return 0, 0, false }
