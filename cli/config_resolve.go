package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"

	apiv1 "github.com/valentinkolb/filegate/api/v1"
	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/runtimecfg"
)

// ConfigSource says where a value's current setting came from. Knowing this is
// what lets an operator tell "I chose 3s" from "3s happens to be the default".
type ConfigSource string

const (
	SourceDefault ConfigSource = "default"
	SourceFile    ConfigSource = "file"
	SourceEnv     ConfigSource = "env"
	SourceRuntime ConfigSource = "runtime"
)

// ResolvedConfig is a configuration plus the provenance of every key.
type ResolvedConfig struct {
	Config  domain.Config
	Sources map[string]ConfigSource
}

// resolveConfig layers the static sources and the runtime overrides, then
// validates the result.
//
// Precedence is runtime override, then environment, then config file, then
// built-in default. Runtime wins because it is the most deliberate: someone
// changed it through an API while the service was running.
func resolveConfig(configFile string, overrides map[string]json.RawMessage) (ResolvedConfig, error) {
	v, err := newConfigViper(configFile)
	if err != nil {
		return ResolvedConfig{}, err
	}

	sources := staticSources(v)
	for path, raw := range overrides {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return ResolvedConfig{}, fmt.Errorf("runtime override %q is not valid JSON: %w", path, err)
		}
		v.Set(path, value)
		sources[path] = SourceRuntime
	}

	cfg, err := finishConfig(v)
	if err != nil {
		return ResolvedConfig{}, err
	}
	if err := validateResolvedConfig(cfg); err != nil {
		return ResolvedConfig{}, err
	}

	return ResolvedConfig{Config: cfg, Sources: sources}, nil
}

// staticSources determines, per key, whether the value came from the config
// file, the environment, or the built-in default.
//
// Viper cannot answer this directly: IsSet is true for defaults as well, so the
// layers are probed individually.
func staticSources(v *viper.Viper) map[string]ConfigSource {
	fileKeys := make(map[string]bool)
	if used := v.ConfigFileUsed(); used != "" {
		// A second viper without defaults reads the file alone, so IsSet on it
		// means "the operator wrote this key" rather than "a default exists".
		bare := viper.New()
		bare.SetConfigFile(used)
		if err := bare.ReadInConfig(); err == nil {
			for _, key := range bare.AllKeys() {
				fileKeys[key] = true
			}
		}
	}

	out := make(map[string]ConfigSource, len(allConfigFlagSpecs()))
	for _, spec := range allConfigFlagSpecs() {
		switch {
		case os.Getenv(envVarFor(spec.Path)) != "":
			out[spec.Path] = SourceEnv
		case fileKeys[spec.Path]:
			out[spec.Path] = SourceFile
		default:
			out[spec.Path] = SourceDefault
		}
	}
	return out
}

// envVarFor mirrors the viper env binding: FILEGATE prefix, dots to underscores.
func envVarFor(path string) string {
	return "FILEGATE_" + strings.ToUpper(strings.NewReplacer(".", "_").Replace(path))
}

// ConfigManager owns the live configuration and applies changes to it.
type ConfigManager struct {
	holder     *domain.ConfigHolder
	store      *runtimecfg.Store
	configFile string

	// booted is the configuration the process actually started with. Static
	// values are compared against it to report what needs a restart, since
	// those are still being used no matter what the sources now say.
	booted  domain.Config
	sources map[string]ConfigSource
}

func newConfigManager(configFile string, store *runtimecfg.Store, resolved ResolvedConfig) *ConfigManager {
	return &ConfigManager{
		holder:     domain.NewConfigHolder(resolved.Config),
		store:      store,
		configFile: configFile,
		booted:     resolved.Config,
		sources:    resolved.Sources,
	}
}

func (m *ConfigManager) Holder() *domain.ConfigHolder { return m.holder }

func (m *ConfigManager) Sources() map[string]ConfigSource {
	out := make(map[string]ConfigSource, len(m.sources))
	for path, source := range m.sources {
		out[path] = source
	}
	return out
}

// Validate resolves a proposed set of changes without applying them, so a UI
// can check input as it is typed.
func (m *ConfigManager) Validate(changes map[string]any) error {
	_, err := m.resolveWith(changes)
	return err
}

func (m *ConfigManager) resolveWith(changes map[string]any) (ResolvedConfig, error) {
	merged := m.store.Overrides()
	for path, value := range changes {
		if value == nil {
			delete(merged, path)
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return ResolvedConfig{}, fmt.Errorf("cannot encode %q: %w", path, err)
		}
		merged[path] = encoded
	}
	return resolveConfig(m.configFile, merged)
}

// Apply validates changes, persists them, and publishes the new snapshot.
//
// Static keys are accepted and stored but cannot take effect until a restart;
// they come back in the returned list rather than being silently ignored, which
// is the failure mode a generic config API falls into most easily.
func (m *ConfigManager) Apply(changes map[string]any) ([]domain.RestartRequired, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	for path := range changes {
		if _, known := specByPath(path); !known {
			return nil, fmt.Errorf("unknown config key %q", path)
		}
	}

	resolved, err := m.resolveWith(changes)
	if err != nil {
		return nil, err
	}
	if err := m.store.SetOverrides(changes); err != nil {
		return nil, err
	}

	m.holder.Set(resolved.Config)
	m.sources = resolved.Sources
	return m.pendingRestarts(resolved.Config), nil
}

// Reload re-reads every source and republishes. Used by SIGHUP and by the
// reload endpoint, so an operator who edited the file by hand can apply it.
func (m *ConfigManager) Reload() ([]domain.RestartRequired, error) {
	resolved, err := resolveConfig(m.configFile, m.store.Overrides())
	if err != nil {
		return nil, err
	}
	m.holder.Set(resolved.Config)
	m.sources = resolved.Sources
	return m.pendingRestarts(resolved.Config), nil
}

// pendingRestarts compares static values in the desired configuration against
// the ones the process is actually running with.
func (m *ConfigManager) pendingRestarts(desired domain.Config) []domain.RestartRequired {
	var out []domain.RestartRequired
	for _, spec := range allConfigFlagSpecs() {
		if spec.Scope != scopeStatic {
			continue
		}
		running := formatConfigValue(&m.booted, spec)
		wanted := formatConfigValue(&desired, spec)
		if running == wanted {
			continue
		}
		out = append(out, domain.RestartRequired{Path: spec.Path, Running: running, Desired: wanted})
	}
	return out
}

func specByPath(path string) (configFlagSpec, bool) {
	for _, spec := range allConfigFlagSpecs() {
		if spec.Path == path {
			return spec, true
		}
	}
	return configFlagSpec{}, false
}

// kindName maps a spec kind to the type name clients use to pick an input
// control. Keeping this next to the resolver means a new kind shows up as an
// unknown type in the API rather than silently rendering as a text field.
func kindName(kind configFlagKind) string {
	switch kind {
	case configFlagBool:
		return "bool"
	case configFlagInt, configFlagInt64:
		return "int"
	case configFlagDuration:
		return "duration"
	case configFlagStringArray:
		return "stringList"
	case configFlagS3Keys:
		return "s3Keys"
	case configFlagRetentionBuckets:
		return "retentionBuckets"
	default:
		return "string"
	}
}

// Schema describes every configuration key so a client can render controls
// without hardcoding the list.
func (m *ConfigManager) Schema() []apiv1.ConfigKeySchema {
	defaults := defaultConfigValues()
	specs := allConfigFlagSpecs()

	out := make([]apiv1.ConfigKeySchema, 0, len(specs))
	for _, spec := range specs {
		entry := apiv1.ConfigKeySchema{
			Path:   spec.Path,
			Type:   kindName(spec.Kind),
			Scope:  spec.Scope.String(),
			Usage:  spec.Usage,
			Reason: spec.Reason,
			Unit:   spec.Unit,
			Secret: spec.Secret,
		}
		// A secret's default is either empty or a placeholder; publishing it
		// would defeat the deny list.
		if !spec.Secret {
			entry.Default = defaults[spec.Path]
		}
		out = append(out, entry)
	}
	return out
}

// Values reports the effective value and provenance of every key.
func (m *ConfigManager) Values() apiv1.ConfigValuesResponse {
	cfg := m.holder.Get()
	sources := m.Sources()
	specs := allConfigFlagSpecs()

	values := make([]apiv1.ConfigValue, 0, len(specs))
	for _, spec := range specs {
		source := sources[spec.Path]
		if source == "" {
			source = SourceDefault
		}
		values = append(values, apiv1.ConfigValue{
			Path:   spec.Path,
			Value:  configValueForAPI(&cfg, spec),
			Source: string(source),
			Scope:  spec.Scope.String(),
		})
	}

	return apiv1.ConfigValuesResponse{
		GeneratedAt:     time.Now().UnixMilli(),
		Values:          values,
		RestartRequired: toAPIRestarts(m.pendingRestarts(cfg)),
	}
}

func toAPIRestarts(in []domain.RestartRequired) []apiv1.ConfigRestartRequired {
	if len(in) == 0 {
		return nil
	}
	out := make([]apiv1.ConfigRestartRequired, 0, len(in))
	for _, entry := range in {
		out = append(out, apiv1.ConfigRestartRequired{Path: entry.Path, Running: entry.Running, Desired: entry.Desired})
	}
	return out
}

// ApplyChanges adapts Apply to the shape the HTTP adapter consumes.
func (m *ConfigManager) ApplyChanges(changes map[string]any) ([]apiv1.ConfigRestartRequired, error) {
	restarts, err := m.Apply(changes)
	if err != nil {
		return nil, err
	}
	return toAPIRestarts(restarts), nil
}

// ValidateChanges checks a batch without applying it.
func (m *ConfigManager) ValidateChanges(changes map[string]any) error {
	return m.Validate(changes)
}

// ReloadConfig re-reads every source and republishes.
func (m *ConfigManager) ReloadConfig() ([]apiv1.ConfigRestartRequired, error) {
	restarts, err := m.Reload()
	if err != nil {
		return nil, err
	}
	return toAPIRestarts(restarts), nil
}

// defaultConfigValues resolves the built-in defaults on their own, with no file
// and no environment, so the schema can show what a key falls back to.
func defaultConfigValues() map[string]any {
	v := viper.New()
	registerConfigDefaults(v)

	out := make(map[string]any)
	for _, spec := range allConfigFlagSpecs() {
		out[spec.Path] = v.Get(spec.Path)
	}
	return out
}
