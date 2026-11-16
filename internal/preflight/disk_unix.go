//go:build unix

package preflight

import "syscall"

// wOK is access(2)'s W_OK.
const wOK = 0x2

// writable reports whether this user may write in dir, as access(2) says:
// nothing is written to find out.
func writable(dir string) error { return syscall.Access(dir, wOK) }

// freeBytes is the space free to this user on dir's filesystem (statfs).
func freeBytes(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
