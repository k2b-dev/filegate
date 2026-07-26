package v1

// Types for the configuration endpoints.
//
// The schema endpoint exists so clients do not hardcode a list of keys: an
// admin UI renders a control per key from its declared type, and a key added in
// a later release appears without a UI change.

// ConfigScope says whether a key can change while the server runs.
const (
	ConfigScopeStatic  = "static"
	ConfigScopeRuntime = "runtime"
)

// ConfigKeySchema describes one configuration key.
type ConfigKeySchema struct {
	Path string `json:"path"`
	// Type is the value shape: string, bool, int, duration, stringList, or a
	// structured kind such as s3Keys. Clients pick their input control from it.
	Type  string `json:"type"`
	Scope string `json:"scope"`
	Usage string `json:"usage"`
	// Reason explains why a static key cannot change at runtime. Empty for
	// runtime keys.
	Reason string `json:"reason,omitempty"`
	// Unit says what a number means: "bytes" for a byte count, empty
	// otherwise. Lets a client render 65536 as 64 KiB rather than as an
	// integer, which the type alone cannot express since a byte limit and a
	// max-count are both ints.
	Unit string `json:"unit,omitempty"`
	// Secret marks values that are never returned; they report presence only.
	Secret bool `json:"secret"`
	// Default is the built-in value used when nothing sets the key.
	Default any `json:"default,omitempty"`
}

// RetentionBucket is one age window of the version retention policy.
//
// A deliberate API shape rather than the domain struct, which carries only
// mapstructure tags and would otherwise serialize as Go field names with
// nanosecond durations.
type RetentionBucket struct {
	// KeepFor is the window measured back from now, as a duration string.
	KeepFor string `json:"keepFor"`
	// MaxCount is how many versions to keep inside the window; -1 is unlimited.
	MaxCount int `json:"maxCount"`
}

// ConfigSchemaResponse is the body of GET /v1/config/schema.
type ConfigSchemaResponse struct {
	Keys []ConfigKeySchema `json:"keys"`
}

// ConfigValue is one key's current state.
type ConfigValue struct {
	Path string `json:"path"`
	// Value is the effective value, or {"configured": bool} for secrets.
	Value any `json:"value"`
	// Source is where the effective value came from: default, file, env or
	// runtime. This is what distinguishes a deliberate setting from a default
	// that happens to match.
	Source string `json:"source"`
	Scope  string `json:"scope"`
}

// ConfigValuesResponse is the body of GET /v1/config.
type ConfigValuesResponse struct {
	GeneratedAt int64         `json:"generatedAt"`
	Values      []ConfigValue `json:"values"`
	// RestartRequired lists static settings whose stored value differs from
	// the one the process is running with.
	RestartRequired []ConfigRestartRequired `json:"restartRequired,omitempty"`
}

// ConfigRestartRequired names a static setting that cannot take effect yet.
type ConfigRestartRequired struct {
	Path    string `json:"path"`
	Running string `json:"running"`
	Desired string `json:"desired"`
}

// ConfigChangeRequest is the body of PATCH /v1/config and POST /v1/config/validate.
//
// Changes arrive as a batch because some settings are only valid together:
// cors.allow_credentials cannot be combined with a wildcard origin, so setting
// them one at a time would have to pass through an invalid state.
type ConfigChangeRequest struct {
	// Changes maps dotted config paths to values. A null value clears the
	// runtime override so the key falls back to the file or its default.
	Changes map[string]any `json:"changes"`
}

// ConfigChangeResponse reports what happened to a change request.
type ConfigChangeResponse struct {
	Applied         bool                    `json:"applied"`
	RestartRequired []ConfigRestartRequired `json:"restartRequired,omitempty"`
}
