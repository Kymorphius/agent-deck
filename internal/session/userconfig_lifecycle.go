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
