package sysinfo

import (
	"math"
	"testing"
)

// matrix/hardened: verify byte totals and rejection boundaries independently of
// the platform's syscall layout, including values that previously wrapped.
func TestDiskUsage(t *testing.T) {
	const maxUint64 = ^uint64(0)
	tests := []struct {
		name      string
		blocks    uint64
		available uint64
		unit      int64
		want      DiskStat
	}{
		{
			name: "available excludes reserved blocks", blocks: 150, available: 100, unit: 4096,
			want: DiskStat{Available: true, UsedBytes: 204800, TotalBytes: 614400, UsagePercent: 100.0 / 3},
		},
		{
			name: "empty filesystem", blocks: 1024, available: 1024, unit: 4096,
			want: DiskStat{Available: true, TotalBytes: 4194304},
		},
		{
			name: "full filesystem", blocks: 1024, unit: 4096,
			want: DiskStat{Available: true, UsedBytes: 4194304, TotalBytes: 4194304, UsagePercent: 100},
		},
		{
			name: "largest exact byte count", blocks: maxUint64, available: maxUint64, unit: 1,
			want: DiskStat{Available: true, TotalBytes: maxUint64},
		},
		{name: "zero unit", blocks: 10, available: 5},
		{name: "negative unit", blocks: 10, available: 5, unit: -4096},
		{name: "zero total", unit: 4096},
		{name: "available exceeds total", blocks: 10, available: 11, unit: 4096},
		{name: "unrepresentable negative available", blocks: 10, available: maxUint64, unit: 4096},
		{name: "byte multiplication overflow", blocks: maxUint64/4096 + 1, unit: 4096},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := diskUsage(tt.blocks, tt.available, tt.unit)
			if got.Available != tt.want.Available || got.UsedBytes != tt.want.UsedBytes || got.TotalBytes != tt.want.TotalBytes || math.Abs(got.UsagePercent-tt.want.UsagePercent) > 1e-10 {
				t.Errorf("diskUsage() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
