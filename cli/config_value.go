package cli

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	apiv1 "github.com/valentinkolb/filegate/api/v1"
	"github.com/valentinkolb/filegate/domain"
)

// configValueByPath reads a config value addressed by its dotted path.
//
// It walks the struct using the mapstructure tags that already define the
// mapping, rather than a hand-written switch over 55 keys. The write path uses
// an explicit switch because it must reject unknown keys loudly, but reading is
// total: every tagged field is reachable, and a renamed field stops resolving
// instead of silently reading the wrong one.
func configValueByPath(cfg *domain.Config, path string) (any, bool) {
	current := reflect.ValueOf(cfg).Elem()

	for _, segment := range strings.Split(path, ".") {
		if current.Kind() != reflect.Struct {
			return nil, false
		}
		field, ok := fieldByMapstructure(current, segment)
		if !ok {
			return nil, false
		}
		current = field
	}
	return current.Interface(), true
}

// copyConfigValue copies one typed field between config snapshots. It is used
// when publishing a manifest: runtime fields move to the live snapshot while
// static fields keep their already-running value until restart.
func copyConfigValue(dst, src *domain.Config, path string) bool {
	dstValue := reflect.ValueOf(dst).Elem()
	srcValue := reflect.ValueOf(src).Elem()
	for _, segment := range strings.Split(path, ".") {
		var ok bool
		dstValue, ok = fieldByMapstructure(dstValue, segment)
		if !ok {
			return false
		}
		srcValue, ok = fieldByMapstructure(srcValue, segment)
		if !ok {
			return false
		}
	}
	if !dstValue.CanSet() || dstValue.Type() != srcValue.Type() {
		return false
	}
	dstValue.Set(srcValue)
	return true
}

func fieldByMapstructure(value reflect.Value, name string) (reflect.Value, bool) {
	structType := value.Type()
	for i := range structType.NumField() {
		tag := structType.Field(i).Tag.Get("mapstructure")
		if tag == "" {
			continue
		}
		if strings.Split(tag, ",")[0] == name {
			return value.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// formatConfigValue renders a value for comparison and display.
//
// Secrets never reach this: callers check spec.Secret first. Durations print in
// their configured form rather than as nanoseconds, so a restart-required diff
// reads as "5m against 30s" instead of two large integers.
func formatConfigValue(cfg *domain.Config, spec configFlagSpec) string {
	value, ok := configValueByPath(cfg, spec.Path)
	if !ok {
		return ""
	}
	switch typed := value.(type) {
	case time.Duration:
		return typed.String()
	case []string:
		return strings.Join(typed, ",")
	default:
		return fmt.Sprintf("%v", value)
	}
}

// configValueForAPI returns a value ready to serialize, with secrets replaced
// by a presence flag.
//
// The endpoints are meant to be complete, so secrecy is decided by the spec's
// deny list rather than by whichever fields someone remembered to include.
func configValueForAPI(cfg *domain.Config, spec configFlagSpec) any {
	value, ok := configValueByPath(cfg, spec.Path)
	if !ok {
		return nil
	}
	if spec.Secret {
		return map[string]any{"configured": !isZeroValue(value)}
	}
	if duration, isDuration := value.(time.Duration); isDuration {
		return duration.String()
	}
	// Retention buckets are published in a deliberate API shape; the domain
	// struct has only mapstructure tags and would serialize as Go field names
	// with nanosecond durations.
	if buckets, isBuckets := value.([]domain.RetentionBucketConfig); isBuckets {
		out := make([]apiv1.RetentionBucket, 0, len(buckets))
		for _, bucket := range buckets {
			out = append(out, apiv1.RetentionBucket{KeepFor: bucket.KeepFor.String(), MaxCount: bucket.MaxCount})
		}
		return out
	}
	return value
}

func isZeroValue(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	return rv.IsZero()
}
