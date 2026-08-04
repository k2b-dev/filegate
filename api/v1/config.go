package v1

// Types for the declarative configuration endpoints.

const (
	ConfigScopeStatic  = "static"
	ConfigScopeRuntime = "runtime"

	ConfigManagedByManifest  = "manifest"
	ConfigManagedByBootstrap = "bootstrap"
	ConfigManagedByResource  = "resource"
)

// ConfigKeySchema describes one configuration key.
type ConfigKeySchema struct {
	Path      string   `json:"path"`
	Type      string   `json:"type"`
	Scope     string   `json:"scope"`
	ManagedBy string   `json:"managedBy"`
	Usage     string   `json:"usage"`
	Reason    string   `json:"reason,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	Choices   []string `json:"choices,omitempty"`
	Secret    bool     `json:"secret"`
	Default   any      `json:"default,omitempty"`
}

// RetentionBucket is one age window of the version retention policy.
type RetentionBucket struct {
	KeepFor  string `json:"keepFor"`
	MaxCount int    `json:"maxCount"`
}

// ConfigSchemaResponse is the body of GET /v1/config/schema.
type ConfigSchemaResponse struct {
	Keys []ConfigKeySchema `json:"keys"`
}

// ConfigValue is one key's desired and effective state.
type ConfigValue struct {
	Path      string `json:"path"`
	Effective any    `json:"effective"`
	Desired   any    `json:"desired"`
	Source    string `json:"source"`
	Scope     string `json:"scope"`
	ManagedBy string `json:"managedBy"`
}

// ConfigManifestStatus identifies the complete desired state last applied.
type ConfigManifestStatus struct {
	Revision  string `json:"revision"`
	AppliedAt int64  `json:"appliedAt"`
	AppliedBy string `json:"appliedBy"`
}

// ConfigValuesResponse is the body of GET /v1/config.
type ConfigValuesResponse struct {
	GeneratedAt     int64                   `json:"generatedAt"`
	Manifest        *ConfigManifestStatus   `json:"manifest,omitempty"`
	Values          []ConfigValue           `json:"values"`
	RestartRequired []ConfigRestartRequired `json:"restartRequired,omitempty"`
}

// ConfigRestartRequired names a static setting that cannot take effect yet.
type ConfigRestartRequired struct {
	Path      string `json:"path"`
	Effective string `json:"effective"`
	Desired   string `json:"desired"`
}

// ConfigManifestPlanRequest carries a complete replacement manifest.
type ConfigManifestPlanRequest struct {
	Values map[string]any `json:"values"`
}

// ConfigManifestApplyRequest applies the complete manifest only if the
// revision observed by plan is still current.
type ConfigManifestApplyRequest struct {
	Values           map[string]any `json:"values"`
	ExpectedRevision string         `json:"expectedRevision"`
}

// ConfigManifestChange describes one difference from the applied manifest.
type ConfigManifestChange struct {
	Path       string `json:"path"`
	Operation  string `json:"operation"`
	Activation string `json:"activation"`
	From       any    `json:"from,omitempty"`
	To         any    `json:"to,omitempty"`
}

// ConfigManifestPlanResponse is returned by plan and apply.
type ConfigManifestPlanResponse struct {
	CurrentRevision  string                  `json:"currentRevision"`
	ProposedRevision string                  `json:"proposedRevision"`
	Changes          []ConfigManifestChange  `json:"changes"`
	RestartRequired  []ConfigRestartRequired `json:"restartRequired,omitempty"`
}

// ConfigManifestApplyResponse reports the newly persisted manifest.
type ConfigManifestApplyResponse struct {
	Manifest        ConfigManifestStatus    `json:"manifest"`
	Changes         []ConfigManifestChange  `json:"changes"`
	RestartRequired []ConfigRestartRequired `json:"restartRequired,omitempty"`
}
