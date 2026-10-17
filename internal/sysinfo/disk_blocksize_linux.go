package sysinfo

import "syscall"

// matrix/hardened: Linux statfs counters are in Frsize units, while Bsize is
// the optimal transfer size. Older filesystems can leave Frsize unset; match
// the statvfs compatibility fallback only for that zero value.
func diskBlockSize(stat *syscall.Statfs_t) int64 {
	if stat.Frsize != 0 {
		return int64(stat.Frsize)
	}
	return int64(stat.Bsize)
}
