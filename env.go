package cf_configuration

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// applyEnvOverlay sets exported fields of dst (a *T) from environment variables.
// For each field, the env key is EnvPrefix + name, where name is the `env` tag
// if present, otherwise the UPPER_SNAKE form of the `json` tag (or field name).
// Empty or unset variables leave the field unchanged (file/defaults win until
// env supplies a value). Later layers (AfterLoad / DSN) still run after this.
func applyEnvOverlay(dst any, prefix string) error {
	if dst == nil {
		return fmt.Errorf("cf_configuration: env overlay destination is nil")
	}
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return fmt.Errorf("cf_configuration: env overlay destination must be a non-nil pointer")
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("cf_configuration: env overlay destination must point to a struct")
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		key, skip := envKey(f)
		if skip {
			continue
		}
		raw, ok := os.LookupEnv(prefix + key)
		if !ok || raw == "" {
			continue
		}
		if err := setFieldFromString(v.Field(i), f, raw); err != nil {
			return fmt.Errorf("cf_configuration: env %s: %w", prefix+key, err)
		}
	}
	return nil
}

func envKey(f reflect.StructField) (key string, skip bool) {
	if tag := f.Tag.Get("env"); tag != "" {
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" {
			return "", true
		}
		if name != "" {
			return name, false
		}
	}
	name := f.Name
	if tag := f.Tag.Get("json"); tag != "" {
		n, _, _ := strings.Cut(tag, ",")
		if n == "-" {
			return "", true
		}
		if n != "" {
			name = n
		}
	}
	return toEnvName(name), false
}

func toEnvName(s string) string {
	s = strings.ReplaceAll(s, "-", "_")
	var b strings.Builder
	for i, r := range s {
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r - ('a' - 'A'))
			continue
		}
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				prev := rune(s[i-1])
				if prev >= 'a' && prev <= 'z' {
					b.WriteByte('_')
				}
			}
			b.WriteRune(r)
			continue
		}
		if r == '_' {
			b.WriteByte('_')
			continue
		}
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// setFieldFromString parses a raw string into a struct field value. It is
// shared by the env overlay and the flag overlay: both layers hand over a raw
// value and this function decides how to decode it for the field's type.
// Pointer-to-scalar fields are allocated on first use so an explicit value
// survives overlay while an omitted one stays nil.
func setFieldFromString(fv reflect.Value, f reflect.StructField, raw string) error {
	if !fv.CanSet() {
		return nil
	}
	if fv.Kind() == reflect.Ptr {
		if fv.IsNil() {
			fv.Set(reflect.New(fv.Type().Elem()))
		}
		return setFieldFromString(fv.Elem(), f, raw)
	}
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
		return nil
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		fv.SetBool(b)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if fv.Type().PkgPath() == "time" && fv.Type().Name() == "Duration" {
			d, err := time.ParseDuration(raw)
			if err != nil {
				// also accept integer seconds
				sec, err2 := strconv.ParseInt(raw, 10, 64)
				if err2 != nil {
					return err
				}
				d = time.Duration(sec) * time.Second
			}
			fv.SetInt(int64(d))
			return nil
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return err
		}
		fv.SetInt(n)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return err
		}
		fv.SetUint(n)
		return nil
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return err
		}
		fv.SetFloat(n)
		return nil
	case reflect.Slice:
		if fv.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported slice type %s", fv.Type())
		}
		parts := splitCSV(raw)
		sl := reflect.MakeSlice(fv.Type(), len(parts), len(parts))
		for i, p := range parts {
			sl.Index(i).SetString(p)
		}
		fv.Set(sl)
		return nil
	default:
		return fmt.Errorf("unsupported field type %s", f.Type)
	}
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
