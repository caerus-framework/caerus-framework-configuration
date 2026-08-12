package cf_configuration

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
)

// applyFlagOverlay sets exported fields of dst (a *T) from parsed flag values.
// For each field the CLI flag name is the `flag` tag (the name after "--");
// `flag:"-"` skips the field and an absent tag means no CLI for that field.
// The overlay runs after env and before AfterLoad; flags that were not provided
// leave the field at its file/env value.
func applyFlagOverlay(dst any, flags map[string]string) error {
	if dst == nil {
		return fmt.Errorf("cf_configuration: flag overlay destination is nil")
	}
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return fmt.Errorf("cf_configuration: flag overlay destination must be a non-nil pointer")
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("cf_configuration: flag overlay destination must point to a struct")
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, ok := flagKey(f)
		if !ok {
			continue
		}
		raw, exists := flags[name]
		if !exists {
			continue
		}
		if err := setFieldFromString(v.Field(i), f, raw); err != nil {
			return fmt.Errorf("cf_configuration: flag --%s: %w", name, err)
		}
	}
	return nil
}

// flagKey returns the CLI flag name for a struct field. The second result is
// false when the field has no `flag` tag, a `-` tag, or an empty name (no CLI).
func flagKey(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("flag")
	if tag == "" {
		return "", false
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" || name == "-" {
		return "", false
	}
	return name, true
}

// flagFieldSupported reports whether setFieldFromString can decode a raw string
// into a field of this type. Unsupported flag-tagged fields are a wiring error
// and fail ParseFlags rather than silently doing nothing.
func flagFieldSupported(f reflect.StructField) bool {
	t := f.Type
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true // int64 includes time.Duration; setFieldFromString handles it
	case reflect.Slice:
		return t.Elem().Kind() == reflect.String
	default:
		return false
	}
}

// splitKnownFlags splits args into (flags to hand to flag.Parse, everything
// else), GNU-style interspersed: known long flags (and their values) are pulled
// out no matter where they appear, while positional args, unknown flags, and
// single-dash args pass through to the remainder in order. Only "--" terminates
// scanning (everything after it is positional). This is what lets subcommands
// (`serve`), positionals (`price get <uuid>`), and app flags
// (e.g. `--my-app-flag`) survive while `serve --vpq-debug` still overlays.
// `known` maps flag name → field kind (reflect.Bool flags take no value).
func splitKnownFlags(args []string, known map[string]reflect.Kind) (parseList, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			rest = append(rest, args[i+1:]...)
			return parseList, rest
		case strings.HasPrefix(a, "--"):
			name := strings.TrimPrefix(a, "--")
			if n, _, hasEq := strings.Cut(name, "="); hasEq {
				name = n
			}
			if _, ok := known[name]; !ok {
				rest = append(rest, a) // unknown flag → remainder, keep scanning
				continue
			}
			parseList = append(parseList, a)
			if !strings.Contains(a, "=") && known[name] != reflect.Bool && i+1 < len(args) {
				i++
				parseList = append(parseList, args[i]) // consume the value
			}
		default:
			rest = append(rest, a) // positional → remainder, keep scanning
		}
	}
	return parseList, rest
}

// flagNamesLocked walks every registered source's struct fields and collects
// flag-tagged names with their kinds. Callers must hold c.mu. Flag names are a
// process-wide namespace: a flag-tagged field of an unsupported type, the same
// flag declared twice on one source, or the same flag declared by two sources
// (including core sources) is a wiring error — even when the types match.
// Silent sharing of one CLI flag across sources is not allowed.
func (c *Configuration) flagNamesLocked() (map[string]reflect.Kind, error) {
	names := make(map[string]reflect.Kind)
	owners := make(map[string]string) // flag name → source that declared it
	for _, name := range c.order {
		s := c.sources[name]
		if s == nil {
			continue
		}
		z := s.zero()
		v := reflect.ValueOf(z)
		if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
			continue
		}
		t := v.Elem().Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, ok := flagKey(f)
			if !ok {
				continue
			}
			if !flagFieldSupported(f) {
				return nil, fmt.Errorf("cf_configuration: flag --%s on %s has unsupported type %s", name, s.name, f.Type)
			}
			if prev, dup := owners[name]; dup {
				if prev == s.name {
					return nil, fmt.Errorf("cf_configuration: flag --%s is declared more than once on source %q", name, s.name)
				}
				return nil, fmt.Errorf("cf_configuration: flag --%s is declared by sources %q and %q", name, prev, s.name)
			}
			owners[name] = s.name
			// Pointer-to-scalar fields register as their element kind so a
			// *bool flag stays a no-value Bool flag.
			kind := f.Type.Kind()
			if f.Type.Kind() == reflect.Ptr {
				kind = f.Type.Elem().Kind()
			}
			names[name] = kind
		}
	}
	// Job flags: each source with a declared job registers --<Flag> as a string
	// flag (CLI-only; the value is read by JobRequests, not a struct field). A
	// collision with a field flag or another source's job flag is a wiring error.
	jobFlags := make(map[string]bool)
	for _, name := range c.order {
		s := c.sources[name]
		if s == nil || s.job.Flag == "" {
			continue
		}
		if owner, dup := owners[s.job.Flag]; dup {
			return nil, fmt.Errorf("cf_configuration: job flag --%s conflicts with a field flag on source %q", s.job.Flag, owner)
		}
		if jobFlags[s.job.Flag] {
			return nil, fmt.Errorf("cf_configuration: job flag --%s is declared by more than one source", s.job.Flag)
		}
		jobFlags[s.job.Flag] = true
		owners[s.job.Flag] = s.name
		names[s.job.Flag] = reflect.String
	}
	return names, nil
}

// ParseFlags registers --<flag> for every currently registered source's
// flag-tagged fields and a --<source-name> file-path flag for every source with
// a Path, parses args, and re-applies the resulting values across all sources
// (flags win over env; env wins over file). The file-path flags override where
// each source's config file is read from; defaults are the sources' current
// paths, so an absent flag is a no-op.
//
// Flags are a process-start overlay: the parsed field map is kept and re-applied
// on every subsequent Reload / ReloadAll; a path override persists on the
// source itself (reloads and the file watcher follow it).
//
// Register all AddSource calls first so the flag definitions exist. Unknown
// flags and positional args are returned untouched — subcommands (`serve`,
// `migrate`) and app flags fall through to the caller.
func (c *Configuration) ParseFlags(args []string) (rest []string, err error) {
	if c == nil {
		return nil, errors.New("cf_configuration: ParseFlags on nil component")
	}
	c.mu.Lock()
	names, err := c.flagNamesLocked()
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	// Per-source file-path flags: --<name> for every source that has a Path.
	// Each shares the source's Name as its flag name; a field flag with the
	// same name (or a Bool one) is a wiring error.
	pathFlags := make(map[string]string)
	for _, name := range c.order {
		s := c.sources[name]
		if s == nil || s.path == "" {
			continue
		}
		if _, dup := names[s.name]; dup {
			c.mu.Unlock()
			return nil, fmt.Errorf("cf_configuration: source %q file-path flag --%s conflicts with a field flag", s.name, s.name)
		}
		names[s.name] = reflect.String
		pathFlags[s.name] = s.path
	}
	if len(names) == 0 {
		c.mu.Unlock()
		return args, nil
	}
	parseList, rest := splitKnownFlags(args, names)

	fs := flag.NewFlagSet("caerus", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for name, kind := range names {
		if kind == reflect.Bool {
			fs.Bool(name, false, "")
		} else {
			fs.String(name, pathFlags[name], "")
		}
	}
	if err := fs.Parse(parseList); err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("cf_configuration: parse flags: %w", err)
	}
	values := make(map[string]string)
	fs.Visit(func(f *flag.Flag) {
		values[f.Name] = f.Value.String()
	})
	if len(values) == 0 {
		c.mu.Unlock()
		return rest, nil
	}

	// Apply file-path overrides first: they change which files subsequent loads
	// read. Absolutize so the watcher and event matching stay consistent.
	for name, v := range values {
		if _, isPath := pathFlags[name]; !isPath || v == "" {
			continue
		}
		s := c.sources[name]
		if s == nil {
			continue
		}
		abs, err := filepath.Abs(v)
		if err != nil {
			c.mu.Unlock()
			return nil, fmt.Errorf("cf_configuration: --%s: %w", name, err)
		}
		if abs == s.path {
			continue
		}
		s.path = abs
		if c.watcher != nil {
			_ = c.watchLocked(s)
		}
	}

	// Field flag overlay (as before): kept so Reload / ReloadAll re-apply it.
	if len(c.flagValues) == 0 {
		c.flagValues = make(map[string]string)
	}
	for name, v := range values {
		if _, isPath := pathFlags[name]; isPath {
			continue
		}
		c.flagValues[name] = v
	}

	targets, err := c.reloadAllLocked()
	c.mu.Unlock()
	for _, t := range targets {
		t.rel.OnConfigReload(t.name, t.cfg)
	}
	return rest, err
}
