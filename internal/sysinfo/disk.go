package sysinfo

import "syscall"

// collectDisk gets root filesystem usage via Statfs (cross-platform).
func collectDisk() DiskStat {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return DiskStat{}
	}

	return diskUsage(stat.Blocks, stat.Bavail, diskBlockSize(&stat))
}

// matrix/hardened: filesystem counters use the platform's allocation unit,
// which can differ from its optimal I/O size. Reject invalid snapshots instead
// of wrapping the byte counts or displaying an apparently healthy percentage.
func diskUsage(blocks, available uint64, blockSize int64) DiskStat {
	if blockSize <= 0 || blocks == 0 || available > blocks {
		return DiskStat{}
	}
	unit := uint64(blockSize)
	if blocks > ^uint64(0)/unit {
		return DiskStat{}
	}

	totalBytes := blocks * unit
	freeBytes := available * unit // Bavail remains space available to non-root.
	usedBytes := totalBytes - freeBytes

	pct := float64(usedBytes) / float64(totalBytes) * 100

	return DiskStat{
		Available:    true,
		UsedBytes:    usedBytes,
		TotalBytes:   totalBytes,
		UsagePercent: pct,
	}
}
