# System resource accounting

<!-- matrix/hardened: document the units and provenance of guest resource metrics. -->

Starting with `1.16.26+matrix.94`, resource collection distinguishes filesystem
allocation units from preferred I/O sizes and preserves guest memory scope.

## Disk

Disk statistics describe the root filesystem visible to Agent Deck. On Linux,
`statfs.Frsize` is the counting unit for `Blocks` and `Bavail`; `Bsize` is used
only if the filesystem reports a zero `Frsize`. On Darwin, `Bsize` is already
the fundamental unit, while `Iosize` describes preferred I/O. No platform or
device-specific correction multiplier is used.

The unit definitions follow the [Linux statvfs manual](https://www.man7.org/linux/man-pages/man3/statvfs.3.html),
the [glibc statfs conversion](https://github.com/bminor/glibc/blob/master/sysdeps/unix/sysv/linux/internal_statvfs.c),
and [Apple's statfs documentation](https://developer.apple.com/library/archive/documentation/System/Conceptual/ManPages_iPhoneOS/man2/statfs.2.html).

Total bytes are `Blocks × unit`. Used bytes retain the existing convention of
`total − Bavail × unit`, so blocks unavailable to an ordinary user count as
used. These filesystem values do not imply an application's private storage
quota. Nonpositive units, zero total capacity, available blocks greater than
total blocks, or byte-count overflow make the metric unavailable.

## Memory

Linux memory uses `MemTotal − MemAvailable`, in the units reported by
`/proc/meminfo`. The explicit `/proc/ish/version` interface marks an iSH/LinPad
guest. Its numbers keep their original meaning and receive a visible `guest`
qualifier in every header format, the system panel, CLI text, remote previews,
and the web footer. The CLI/REST memory object includes `"scope": "guest"`.

In the current iOS LinPad runtime, this guest total is the application memory
budget constrained by the operating system, not the iPad's physical RAM. Its
reported used value is budget usage, not total system usage. Agent Deck does
not infer host RAM from `Active`/`Inactive`, device models, or fixed sizes.
It does not read the guest's process/OOM diagnostics to identify the scope.

Missing required fields, malformed numbers or units, zero totals,
available memory above total, and byte-count overflow make Linux memory
unavailable. The collector does not fabricate a percentage from partial data.

Ordinary Linux/macOS displays remain unchanged. An absent scope field from
older remotes preserves their existing display; an older controller may
ignore the new optional field. Deploy a scope-aware controller when viewing
guest memory remotely.

## Validation boundaries

Collection and rendering regressions run through `scripts/run-isolated.py`.
Platform-specific Linux/Darwin tests check their own syscall field semantics;
Linux ARM64 static builds can be staged without replacing a macOS binary.
A successful cross-build is not device acceptance. Native readback of
`system stats --json`, comparison with guest `df`, and the visible iPad header
remain deployment checks for the device operator.
