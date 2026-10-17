//go:build !linux

package sysinfo

import "syscall"

// matrix/hardened: Darwin statfs Bsize is the fundamental counter unit;
// Iosize describes optimal transfers and must not scale the block counts.
func diskBlockSize(stat *syscall.Statfs_t) int64 {
	return int64(stat.Bsize)
}
