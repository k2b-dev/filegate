package cli

import (
	"fmt"
	"reflect"
	"strings"
	"time"

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
	return value
}

func isZeroValue(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	return rv.IsZero()
}
