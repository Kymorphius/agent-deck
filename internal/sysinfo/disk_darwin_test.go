package sysinfo

import (
	"syscall"
	"testing"
)

// matrix/hardened: Darwin already reports counter units in Bsize, so a larger
// Iosize must leave both its existing byte totals and usage unchanged.
func TestDiskDarwinFundamentalUnits(t *testing.T) {
	stat := syscall.Statfs_t{Bsize: 4096, Iosize: 1 << 20, Blocks: 12345, Bavail: 6000}
	got := diskUsage(stat.Blocks, stat.Bavail, diskBlockSize(&stat))
	if !got.Available || got.TotalBytes != 50565120 || got.UsedBytes != 25989120 {
		t.Fatalf("fundamental-unit disk usage = %+v", got)
	}
}
