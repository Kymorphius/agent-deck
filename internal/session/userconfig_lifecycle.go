package session

import (
	"fmt"
	"time"
)

// matrix/hardened: lifecycle policy is opt-in and independent of group queues
// and the legacy output-inactivity timeout.
type LifecycleSettings struct {
	AutoSleep  bool   `toml:"auto_sleep,omitempty"`
	SleepAfter string `toml:"sleep_after,omitempty"`
	// matrix/hardened: archive long-dead (error/stopped) sessions; opt-in.
	AutoArchive  bool   `toml:"auto_archive,omitempty"`
	ArchiveAfter string `toml:"archive_after,omitempty"`
	// matrix/hardened: restart quiet Codex hosts that predate the desktop
	// app's Computer Use plugin; opt-in.
	AutoRefreshComputerUse bool `toml:"auto_refresh_computer_use,omitempty"`
}

func (s LifecycleSettings) SleepDuration() (time.Duration, error) {
	if s.SleepAfter == "" {
		return time.Hour, nil
	}
	d, err := time.ParseDuration(s.SleepAfter)
	if err != nil || d < time.Minute || d > 30*24*time.Hour {
		return 0, fmt.Errorf("lifecycle.sleep_after must be between 1m and 720h")
	}
	return d, nil
}

// ArchiveDuration is the time since a dead session's last use before
// auto-archive hides it. Default 24h.
func (s LifecycleSettings) ArchiveDuration() (time.Duration, error) {
	if s.ArchiveAfter == "" {
		return 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s.ArchiveAfter)
	if err != nil || d < time.Hour || d > 365*24*time.Hour {
		return 0, fmt.Errorf("lifecycle.archive_after must be between 1h and 8760h")
	}
	return d, nil
}
