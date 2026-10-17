package sysinfo

import (
	"syscall"
	"testing"
)

// matrix/hardened: an emulated Linux filesystem may report a large optimal
// transfer size while its counters still use ordinary allocation fragments.
func TestDiskLinuxFragmentUnits(t *testing.T) {
	stat := syscall.Statfs_t{Bsize: 1 << 20, Frsize: 4096, Blocks: 12345, Bavail: 6000}
	got := diskUsage(stat.Blocks, stat.Bavail, diskBlockSize(&stat))
	if !got.Available || got.TotalBytes != 50565120 || got.UsedBytes != 25989120 {
		t.Fatalf("fragment-based disk usage = %+v", got)
	}
}

// matrix/hardened: preserve ordinary Linux and legacy zero-Frsize behavior.
func TestDiskLinuxBlockSize(t *testing.T) {
	for _, tt := range []struct {
		name string
		stat syscall.Statfs_t
		want int64
	}{
		{"ordinary filesystem", syscall.Statfs_t{Bsize: 4096, Frsize: 4096}, 4096},
		{"legacy unset fragment", syscall.Statfs_t{Bsize: 4096}, 4096},
		{"zero sizes", syscall.Statfs_t{}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := diskBlockSize(&tt.stat); got != tt.want {
				t.Errorf("diskBlockSize() = %d, want %d", got, tt.want)
			}
		})
	}
}
