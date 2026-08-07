package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/viper"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/runtimecfg"
)

// ConfigSource says where a desired value came from.
type ConfigSource string

const (
	SourceDefault  ConfigSource = "default"
	SourceFile     ConfigSource = "file"
	SourceEnv      ConfigSource = "env"
	SourceManifest ConfigSource = "manifest"
)

// ResolvedConfig is a configuration plus the provenance of every key.
type ResolvedConfig struct {
	Config  domain.Config
	Sources map[string]ConfigSource
}

// resolveConfig layers a complete applied manifest over the bootstrap sources.
//
// A manifest contains only explicitly managed paths. Omission is meaningful:
// it removes that path from managed state and lets it fall back to environment,
// bootstrap file, or the built-in default.
func resolveConfig(configFile string, manifest map[string]any) (ResolvedConfig, error) {
	v, err := newConfigViper(configFile)
	if err != nil {
		return ResolvedConfig{}, err
	}

	sources := staticSources(v)
	for path, value := range manifest {
		v.Set(path, value)
		sources[path] = SourceManifest
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

func staticSources(v *viper.Viper) map[string]ConfigSource {
	fileKeys := make(map[string]bool)
	if used := v.ConfigFileUsed(); used != "" {
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

func envVarFor(path string) string {
	return "FILEGATE_" + strings.ToUpper(strings.NewReplacer(".", "_").Replace(path))
}

// ConfigManager owns the effective runtime snapshot and the separately tracked
// desired manifest state.
type ConfigManager struct {
	holder     *domain.ConfigHolder
	store      *runtimecfg.Store
	configFile string

	mu       sync.RWMutex
	baseline ResolvedConfig
	desired  domain.Config
	sources  map[string]ConfigSource
	manifest *runtimecfg.AppliedManifest
}

func newConfigManager(configFile string, store *runtimecfg.Store, baseline, resolved ResolvedConfig, manifest *runtimecfg.AppliedManifest) *ConfigManager {
	return &ConfigManager{
		holder:     domain.NewConfigHolder(resolved.Config),
		store:      store,
		configFile: configFile,
		baseline: ResolvedConfig{
			Config:  baseline.Config,
			Sources: cloneSources(baseline.Sources),
		},
		desired:  resolved.Config,
		sources:  resolved.Sources,
		manifest: cloneManifest(manifest),
	}
}

func (m *ConfigManager) Holder() *domain.ConfigHolder { return m.holder }

func (m *ConfigManager) Sources() map[string]ConfigSource {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneSources(m.sources)
}

func cloneSources(in map[string]ConfigSource) map[string]ConfigSource {
	out := make(map[string]ConfigSource, len(in))
	for path, source := range in {
		out[path] = source
	}
	return out
}

func cloneManifest(in *runtimecfg.AppliedManifest) *runtimecfg.AppliedManifest {
	if in == nil {
		return nil
	}
	out := *in
	out.Values = cloneValues(in.Values)
	return &out
}

func cloneValues(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	encoded, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(encoded, &out)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

func (m *ConfigManager) PlanManifest(values map[string]any) (apiv1.ConfigManifestPlanResponse, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	plan, _, _, err := m.planLocked(values)
	return plan, err
}

func (m *ConfigManager) ApplyManifest(values map[string]any, expectedRevision, actor string) (apiv1.ConfigManifestApplyResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	currentRevision := ""
	if m.manifest != nil {
		currentRevision = m.manifest.Revision
	}
	if expectedRevision != currentRevision {
		return apiv1.ConfigManifestApplyResponse{}, fmt.Errorf(
			"%w: manifest revision changed: expected %q, current %q; run plan again",
			domain.ErrConflict, expectedRevision, currentRevision,
		)
	}

	plan, normalized, resolved, err := m.planLocked(values)
	if err != nil {
		return apiv1.ConfigManifestApplyResponse{}, err
	}
	appliedAt := time.Now().UTC().UnixMilli()
	if strings.TrimSpace(actor) == "" {
		actor = "bearer-token"
	}
	record := runtimecfg.AppliedManifest{
		Values:    normalized,
		Revision:  plan.ProposedRevision,
		AppliedAt: appliedAt,
		AppliedBy: actor,
	}
	effective := m.holder.Get()
	for _, spec := range allConfigFlagSpecs() {
		if spec.Scope == scopeRuntime {
			if !copyConfigValue(&effective, &resolved.Config, spec.Path) {
				return apiv1.ConfigManifestApplyResponse{}, fmt.Errorf("cannot publish config key %q", spec.Path)
			}
		}
	}
	if err := m.store.SetManifest(record); err != nil {
		return apiv1.ConfigManifestApplyResponse{}, err
	}
	m.holder.Set(effective)
	m.desired = resolved.Config
	m.sources = resolved.Sources
	m.manifest = cloneManifest(&record)

	return apiv1.ConfigManifestApplyResponse{
		Manifest: apiv1.ConfigManifestStatus{
			Revision:  record.Revision,
			AppliedAt: record.AppliedAt,
			AppliedBy: record.AppliedBy,
		},
		Changes:         plan.Changes,
		RestartRequired: toAPIRestarts(m.pendingRestartsLocked(effective, resolved.Config)),
	}, nil
}

func (m *ConfigManager) planLocked(values map[string]any) (apiv1.ConfigManifestPlanResponse, map[string]any, ResolvedConfig, error) {
	normalized, resolved, err := m.normalizeManifest(values)
	if err != nil {
		return apiv1.ConfigManifestPlanResponse{}, nil, ResolvedConfig{}, err
	}

	current := map[string]any{}
	currentRevision := ""
	if m.manifest != nil {
		current = m.manifest.Values
		currentRevision = m.manifest.Revision
	}
	changes := diffManifest(current, normalized)
	effective := m.holder.Get()
	return apiv1.ConfigManifestPlanResponse{
		CurrentRevision:  currentRevision,
		ProposedRevision: manifestRevision(normalized),
		Changes:          changes,
		RestartRequired:  toAPIRestarts(m.pendingRestartsLocked(effective, resolved.Config)),
	}, normalized, resolved, nil
}

func (m *ConfigManager) normalizeManifest(values map[string]any) (map[string]any, ResolvedConfig, error) {
	if values == nil {
		values = map[string]any{}
	}
	for path, value := range values {
		spec, known := specByPath(path)
		if !known {
			return nil, ResolvedConfig{}, fmt.Errorf("unknown config key %q", path)
		}
		if managedBy(spec) != apiv1.ConfigManagedByManifest {
			return nil, ResolvedConfig{}, fmt.Errorf(
				"config key %q is managed by %s, not by the manifest",
				path, managedBy(spec),
			)
		}
		if value == nil {
			return nil, ResolvedConfig{}, fmt.Errorf("config key %q cannot be null; omit it to remove it from the manifest", path)
		}
	}

	decoded, err := resolveConfig(m.configFile, values)
	if err != nil {
		return nil, ResolvedConfig{}, err
	}
	resolved := ResolvedConfig{
		Config:  m.baseline.Config,
		Sources: cloneSources(m.baseline.Sources),
	}
	for path := range values {
		if !copyConfigValue(&resolved.Config, &decoded.Config, path) {
			return nil, ResolvedConfig{}, fmt.Errorf("cannot resolve config key %q", path)
		}
		resolved.Sources[path] = SourceManifest
	}
	if err := validateResolvedConfig(resolved.Config); err != nil {
		return nil, ResolvedConfig{}, err
	}
	normalized := make(map[string]any, len(values))
	for path := range values {
		spec, _ := specByPath(path)
		normalized[path] = configValueForAPI(&resolved.Config, spec)
	}
	return normalized, resolved, nil
}

func manifestRevision(values map[string]any) string {
	encoded, _ := json.Marshal(values)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func diffManifest(current, proposed map[string]any) []apiv1.ConfigManifestChange {
	paths := make(map[string]struct{}, len(current)+len(proposed))
	for path := range current {
		paths[path] = struct{}{}
	}
	for path := range proposed {
		paths[path] = struct{}{}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)

	out := make([]apiv1.ConfigManifestChange, 0)
	for _, path := range ordered {
		before, hadBefore := current[path]
		after, hasAfter := proposed[path]
		if hadBefore && hasAfter && reflect.DeepEqual(before, after) {
			continue
		}
		spec, _ := specByPath(path)
		change := apiv1.ConfigManifestChange{
			Path:       path,
			Activation: spec.Scope.String(),
		}
		switch {
		case !hadBefore:
			change.Operation = "add"
			change.To = after
		case !hasAfter:
			change.Operation = "remove"
			change.From = before
		default:
			change.Operation = "change"
			change.From = before
			change.To = after
		}
		out = append(out, change)
	}
	return out
}

func (m *ConfigManager) pendingRestartsLocked(effective, desired domain.Config) []domain.RestartRequired {
	var out []domain.RestartRequired
	for _, spec := range allConfigFlagSpecs() {
		if spec.Scope != scopeStatic {
			continue
		}
		running := formatConfigValue(&effective, spec)
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

func managedBy(spec configFlagSpec) string {
	switch spec.Path {
	case "s3.access_key", "s3.secret_key", "s3.keys":
		return apiv1.ConfigManagedByResource
	case "storage.runtime_config_path":
		return apiv1.ConfigManagedByBootstrap
	default:
		if spec.Secret {
			return apiv1.ConfigManagedByBootstrap
		}
		return apiv1.ConfigManagedByManifest
	}
}

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

func (m *ConfigManager) Schema() []apiv1.ConfigKeySchema { return configSchema() }

func configSchema() []apiv1.ConfigKeySchema {
	defaults := defaultConfigValues()
	specs := allConfigFlagSpecs()
	out := make([]apiv1.ConfigKeySchema, 0, len(specs))
	for _, spec := range specs {
		entry := apiv1.ConfigKeySchema{
			Path:      spec.Path,
			Type:      kindName(spec.Kind),
			Scope:     spec.Scope.String(),
			ManagedBy: managedBy(spec),
			Usage:     spec.Usage,
			Reason:    spec.Reason,
			Unit:      spec.Unit,
			Choices:   append([]string(nil), spec.Choices...),
			Secret:    spec.Secret,
		}
		if !spec.Secret && !spec.DynamicDefault {
			entry.Default = defaults[spec.Path]
		}
		out = append(out, entry)
	}
	return out
}

func (m *ConfigManager) Values() apiv1.ConfigValuesResponse {
	m.mu.RLock()
	defer m.mu.RUnlock()

	effective := m.holder.Get()
	values := make([]apiv1.ConfigValue, 0, len(allConfigFlagSpecs()))
	for _, spec := range allConfigFlagSpecs() {
		source := m.sources[spec.Path]
		if source == "" {
			source = SourceDefault
		}
		values = append(values, apiv1.ConfigValue{
			Path:      spec.Path,
			Effective: configValueForAPI(&effective, spec),
			Desired:   configValueForAPI(&m.desired, spec),
			Source:    string(source),
			Scope:     spec.Scope.String(),
			ManagedBy: managedBy(spec),
		})
	}

	var status *apiv1.ConfigManifestStatus
	if m.manifest != nil {
		status = &apiv1.ConfigManifestStatus{
			Revision:  m.manifest.Revision,
			AppliedAt: m.manifest.AppliedAt,
			AppliedBy: m.manifest.AppliedBy,
		}
	}
	return apiv1.ConfigValuesResponse{
		GeneratedAt:     time.Now().UnixMilli(),
		Manifest:        status,
		Values:          values,
		RestartRequired: toAPIRestarts(m.pendingRestartsLocked(effective, m.desired)),
	}
}

func toAPIRestarts(in []domain.RestartRequired) []apiv1.ConfigRestartRequired {
	if len(in) == 0 {
		return nil
	}
	out := make([]apiv1.ConfigRestartRequired, 0, len(in))
	for _, entry := range in {
		out = append(out, apiv1.ConfigRestartRequired{
			Path:      entry.Path,
			Effective: entry.Running,
			Desired:   entry.Desired,
		})
	}
	return out
}

func defaultConfigValues() map[string]any {
	v := viper.New()
	registerConfigDefaults(v)

	out := make(map[string]any)
	for _, spec := range allConfigFlagSpecs() {
		out[spec.Path] = v.Get(spec.Path)
	}
	return out
}
