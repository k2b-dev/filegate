package domain

import (
	"sync/atomic"
)

// ConfigHolder publishes the live configuration.
//
// Runtime-scoped consumers read from here on every request instead of capturing
// values when they are constructed, which is what makes a change take effect
// without a restart. Static-scoped values are still read once during startup;
// see the Scope field on the CLI's config specs for which is which.
//
// Reads are lock-free and safe from any goroutine. A manifest apply swaps the
// whole runtime configuration at once, so a request never sees half an update.
type ConfigHolder struct {
	current atomic.Pointer[Config]
}

// NewConfigHolder publishes an initial configuration.
func NewConfigHolder(cfg Config) *ConfigHolder {
	h := &ConfigHolder{}
	h.current.Store(&cfg)
	return h
}

// Get returns the live configuration.
//
// The returned value is a copy, so a caller cannot mutate what other goroutines
// are reading. Slices inside it are shared, which is safe because nothing
// writes to them after a snapshot is published.
func (h *ConfigHolder) Get() Config {
	if h == nil {
		return Config{}
	}
	cfg := h.current.Load()
	if cfg == nil {
		return Config{}
	}
	return *cfg
}

// Set publishes a new configuration. Callers must validate before publishing;
// this is deliberately dumb so that validation lives in one place.
func (h *ConfigHolder) Set(cfg Config) {
	if h == nil {
		return
	}
	h.current.Store(&cfg)
}

// RestartRequired lists the static settings whose desired value differs from
// what the running process is using.
//
// Reporting this is the difference between an honest apply and one that
// accepts desired state, answers success, and quietly changes nothing. The caller
// supplies the comparison because only it knows which paths are static.
type RestartRequired struct {
	Path    string `json:"path"`
	Running string `json:"running"`
	Desired string `json:"desired"`
}
