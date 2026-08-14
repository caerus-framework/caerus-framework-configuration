package cf_configuration

import (
	"reflect"
	"strings"

	cf_logs "github.com/caerus-framework/caerus-framework-logs"
)

// Secret tag convention (this module owns the name; logs owns the appearance).
//
//	Password string `json:"password" env:"PASSWORD" secret:"redact"`
//
// Overlay, Get, and Lookup are unchanged: the live value still holds the
// password. Only helpers in this file look at the tag, for logs and tests.
const (
	// SecretTag is the struct-tag key.
	SecretTag = "secret"
	// SecretRedact is the only supported tag value: print [redacted] / presence,
	// never the cleartext, when using LogArgs / SecretPresence.
	SecretRedact = "redact"
)

// LogArgs returns slog key/value pairs for a config struct (or pointer).
// Top-level exported fields only (same limit as env/flag overlay).
//
//   - Unmarked scalars stay visible (host, port, …).
//   - Fields tagged `secret:"redact"` become RedactedString plus `<json>_set`.
//   - Nested structs are skipped. Do not slog.Any the raw struct instead.
func LogArgs(cfg any) []any {
	v, ok := structValue(cfg)
	if !ok {
		return nil
	}
	t := v.Type()
	var args []any
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := jsonFieldName(f)
		if name == "" {
			continue
		}
		fv := v.Field(i)
		if isSecretRedact(f) {
			s, ok := stringField(fv)
			if !ok {
				continue
			}
			args = append(args, name+"_set", s != "")
			if s != "" {
				args = append(args, name, cf_logs.RedactedString(s))
			}
			continue
		}
		if kv, ok := scalarArgs(name, fv); ok {
			args = append(args, kv...)
		}
	}
	return args
}

// SecretPresence returns only `<json>_set` bools for `secret:"redact"` string
// fields. Use on reload summaries when you already log host/port yourself.
func SecretPresence(cfg any) []any {
	v, ok := structValue(cfg)
	if !ok {
		return nil
	}
	t := v.Type()
	var args []any
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() || !isSecretRedact(f) {
			continue
		}
		name := jsonFieldName(f)
		if name == "" {
			continue
		}
		s, ok := stringField(v.Field(i))
		if !ok {
			continue
		}
		args = append(args, name+"_set", s != "")
	}
	return args
}

func isSecretRedact(f reflect.StructField) bool {
	return f.Tag.Get(SecretTag) == SecretRedact
}

func jsonFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" {
		return ""
	}
	if name != "" {
		return name
	}
	return f.Name
}

func structValue(cfg any) (reflect.Value, bool) {
	if cfg == nil {
		return reflect.Value{}, false
	}
	v := reflect.ValueOf(cfg)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return reflect.Value{}, false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	return v, true
}

func stringField(fv reflect.Value) (string, bool) {
	if fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return "", true
		}
		fv = fv.Elem()
	}
	if fv.Kind() != reflect.String {
		return "", false
	}
	return fv.String(), true
}

func scalarArgs(name string, fv reflect.Value) ([]any, bool) {
	if fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return nil, false
		}
		fv = fv.Elem()
	}
	switch fv.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return []any{name, fv.Interface()}, true
	case reflect.Slice:
		if fv.Type().Elem().Kind() == reflect.String {
			return []any{name, fv.Interface()}, true
		}
	}
	return nil, false
}
