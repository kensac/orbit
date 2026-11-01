package engine

import (
	"sync"
	"time"
)

// ConfigStatus is whether the configuration on disk loads, as the API's
// health reports it. It mirrors the orbit_config_valid gauge, with the error.
type ConfigStatus struct {
	// Valid is false while the newest content on disk fails to load; the
	// previous configuration stays in force meanwhile.
	Valid bool `json:"valid"`
	// LoadedAt is when the configuration in force was loaded.
	LoadedAt time.Time `json:"loaded_at"`
	// LastError is the most recent load failure since this process started,
	// kept after a later load succeeds (LastErrorAt before LoadedAt).
	LastError   *string    `json:"last_error"`
	LastErrorAt *time.Time `json:"last_error_at"`
}

// ConfigState records load outcomes for ConfigStatus. Safe for concurrent use.
type ConfigState struct {
	mu sync.Mutex
	s  ConfigStatus
}

// Loaded records a configuration put in force at t.
func (c *ConfigState) Loaded(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.s.Valid, c.s.LoadedAt = true, t
}

// Failed records a load that failed at t.
func (c *ConfigState) Failed(err error, t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	msg := err.Error()
	c.s.Valid, c.s.LastError, c.s.LastErrorAt = false, &msg, &t
}

// Status returns the current status.
func (c *ConfigState) Status() ConfigStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.s
}
