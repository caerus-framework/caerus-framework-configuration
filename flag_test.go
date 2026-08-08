package cf_configuration

import (
	"reflect"
	"strings"
	"testing"
)

func mustAdd[T any](t *testing.T, c *Configuration, src Source[T]) {
	t.Helper()
	if err := AddSource(c, src); err != nil {
		t.Fatalf("AddSource(%q): %v", src.Name, err)
	}
}

type flagSample struct {
	Host string `json:"host" env:"HOST" flag:"host"`
	Port int    `json:"port" env:"PORT" flag:"port"`
	Log  bool   `json:"log" env:"LOG" flag:"log"`
	Skip string `json:"skip" flag:"-"`
	Only string `json:"only" env:"ONLY"` // no flag tag → no CLI
}

func newFlagSampleSource(t *testing.T, c *Configuration, file string) {
	t.Helper()
	dir := t.TempDir()
	var src Source[flagSample]
	if file != "" {
		src = Source[flagSample]{Name: "app", Path: writeFile(t, dir, "cfg.json", file), Format: FormatJSON, EnvPrefix: "APP_"}
	} else {
		src = Source[flagSample]{Name: "app", EnvPrefix: "APP_"}
	}
	mustAdd(t, c, src)
}

func TestParseFlagsFlagWinsOverEnvAndFile(t *testing.T) {
	t.Setenv("APP_HOST", "env-host")
	t.Setenv("APP_PORT", "99")
	c := New()
	newFlagSampleSource(t, c, `{"host":"file-host","port":1}`)

	rest, err := c.ParseFlags([]string{"--host", "flag-host", "--port", "27018", "serve", "--migrate-db"})
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got := MustGet[flagSample](c, "app")
	if got.Host != "flag-host" || got.Port != 27018 {
		t.Fatalf("got %+v, want flag overlay to win over env and file", got)
	}
	if len(rest) != 2 || rest[0] != "serve" || rest[1] != "--migrate-db" {
		t.Fatalf("rest = %v, want [serve --migrate-db]", rest)
	}
}

func TestParseFlagsEnvWinsOverFileWithoutFlag(t *testing.T) {
	t.Setenv("APP_HOST", "env-host")
	c := New()
	newFlagSampleSource(t, c, `{"host":"file-host","port":1}`)

	if _, err := c.ParseFlags([]string{"--port", "27018"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got := MustGet[flagSample](c, "app")
	if got.Host != "env-host" {
		t.Fatalf("Host = %q, want env overlay to win when the flag is absent", got.Host)
	}
	if got.Port != 27018 {
		t.Fatalf("Port = %d, want the flag", got.Port)
	}
}

func TestParseFlagsBareBool(t *testing.T) {
	t.Setenv("APP_LOG", "false")
	c := New()
	newFlagSampleSource(t, c, "")

	if _, err := c.ParseFlags([]string{"--log"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if !MustGet[flagSample](c, "app").Log {
		t.Fatal("bare --log should set the bool field true")
	}
	if _, err := c.ParseFlags([]string{"--log=false"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if MustGet[flagSample](c, "app").Log {
		t.Fatal("--log=false should clear the bool field")
	}
}

func TestParseFlagsDashDashAndUnknownStayInRest(t *testing.T) {
	c := New()
	newFlagSampleSource(t, c, "")

	// Unknown flags and positional args (subcommand) must survive untouched.
	rest, err := c.ParseFlags([]string{"--skip", "x", "serve", "--migrate-db"})
	if err != nil {
		t.Fatalf("unknown flag must not error, got %v", err)
	}
	if len(rest) != 4 || rest[0] != "--skip" || rest[1] != "x" || rest[2] != "serve" || rest[3] != "--migrate-db" {
		t.Fatalf("rest = %v", rest)
	}
	if got := MustGet[flagSample](c, "app"); got.Skip != "" {
		t.Fatalf("flag:\"-\" field must never be set from CLI, got %q", got.Skip)
	}

	// "--" moves everything after it to rest.
	rest, err = c.ParseFlags([]string{"--", "--port", "27018"})
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if len(rest) != 2 || rest[0] != "--port" || rest[1] != "27018" {
		t.Fatalf("rest after -- = %v", rest)
	}
}

func TestParseFlagsInterspersedAfterSubcommand(t *testing.T) {
	t.Setenv("APP_HOST", "env-host")
	t.Setenv("APP_PORT", "99")
	c := New()
	newFlagSampleSource(t, c, `{"host":"file-host","port":1}`)

	// Flags after a subcommand/positional must still overlay (GNU-style), and
	// order in the remainder is preserved.
	rest, err := c.ParseFlags([]string{"serve", "--host", "flag-host", "get", "--migrate-db"})
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got := MustGet[flagSample](c, "app")
	if got.Host != "flag-host" {
		t.Fatalf("Host = %q, want interspersed flag overlay to win", got.Host)
	}
	if got.Port != 99 {
		t.Fatalf("Port = %d, want env value when its flag is absent", got.Port)
	}
	if len(rest) != 3 || rest[0] != "serve" || rest[1] != "get" || rest[2] != "--migrate-db" {
		t.Fatalf("rest = %v, want [serve get --migrate-db]", rest)
	}
}

func TestParseFlagsMissingFlagLeavesPriorValue(t *testing.T) {
	t.Setenv("APP_PORT", "99")
	c := New()
	newFlagSampleSource(t, c, `{"host":"file-host","port":1}`)

	if _, err := c.ParseFlags([]string{"--host", "cli-host"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got := MustGet[flagSample](c, "app")
	if got.Port != 99 {
		t.Fatalf("Port = %d, want env value preserved when its flag is absent", got.Port)
	}
	if got.Host != "cli-host" {
		t.Fatalf("Host = %q", got.Host)
	}
}

func TestParseFlagsAfterLoadSeesFlags(t *testing.T) {
	c := New()
	mustAdd(t, c, Source[flagSample]{
		Name: "app", EnvPrefix: "APP_",
		AfterLoad: func(v *flagSample) error {
			v.Host = "after-" + v.Host
			return nil
		},
	})
	if _, err := c.ParseFlags([]string{"--host", "cli"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if got := MustGet[flagSample](c, "app"); got.Host != "after-cli" {
		t.Fatalf("Host = %q, want AfterLoad to run after the flag overlay", got.Host)
	}
}

func TestParseFlagsFlagsSurviveReload(t *testing.T) {
	t.Setenv("APP_PORT", "99")
	c := New()
	newFlagSampleSource(t, c, `{"host":"file-host","port":1}`)

	if _, err := c.ParseFlags([]string{"--port", "27018"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if got := MustGet[flagSample](c, "app"); got.Port != 27018 {
		t.Fatalf("Port = %d", got.Port)
	}
	// A forced reload re-applies the same parsed flag values.
	if err := c.Reload("app"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := MustGet[flagSample](c, "app"); got.Port != 27018 {
		t.Fatalf("Port after reload = %d, want flag value to persist", got.Port)
	}
}

func TestParseFlagsInvalidValueErrors(t *testing.T) {
	c := New()
	newFlagSampleSource(t, c, "")
	if _, err := c.ParseFlags([]string{"--port", "abc"}); err == nil {
		t.Fatal("expected an error for a non-integer flag value")
	}
}

func TestParseFlagsUnsupportedFieldTypeErrors(t *testing.T) {
	type bad struct {
		Blob struct{} `flag:"blob"`
	}
	c := New()
	mustAdd(t, c, Source[bad]{Name: "bad", EnvPrefix: "BAD_"})
	if _, err := c.ParseFlags([]string{"--blob", "x"}); err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("err = %v, want unsupported type error", err)
	}
}

func TestParseFlagsSourcePathFlagOverridesFile(t *testing.T) {
	c := New()
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	mustAdd(t, c, Source[flagSample]{
		Name:   "app",
		Path:   writeFile(t, dir1, "cfg.json", `{"host":"default-file"}`),
		Format: FormatJSON,
	})
	if got := MustGet[flagSample](c, "app").Host; got != "default-file" {
		t.Fatalf("Host = %q, want the AddSource default file", got)
	}

	other := writeFile(t, dir2, "override.json", `{"host":"override-file"}`)
	rest, err := c.ParseFlags([]string{"--app", other, "serve"})
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if len(rest) != 1 || rest[0] != "serve" {
		t.Fatalf("rest = %v, want [serve]", rest)
	}
	if got := MustGet[flagSample](c, "app").Host; got != "override-file" {
		t.Fatalf("Host = %q, want the file from --app", got)
	}

	// The override persists: a forced reload keeps reading the new file.
	if err := c.Reload("app"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := MustGet[flagSample](c, "app").Host; got != "override-file" {
		t.Fatalf("Host after reload = %q, want the overridden path to persist", got)
	}
}

func TestParseFlagsSourcePathFlagConflictsWithFieldFlag(t *testing.T) {
	type clash struct {
		V string `json:"v" flag:"app"`
	}
	c := New()
	mustAdd(t, c, Source[clash]{Name: "app", Path: writeFile(t, t.TempDir(), "c.json", `{"v":"x"}`), Format: FormatJSON})
	if _, err := c.ParseFlags(nil); err == nil || !strings.Contains(err.Error(), "conflicts with a field flag") {
		t.Fatalf("err = %v, want a file-path/field flag conflict error", err)
	}
}

func TestParseFlagsDuplicateFieldFlagAcrossSources(t *testing.T) {
	type a struct {
		Host string `json:"host" flag:"host"`
	}
	type b struct {
		Host string `json:"host" flag:"host"`
	}
	dir := t.TempDir()
	c := New()
	mustAdd(t, c, Source[a]{Name: "logs", Path: writeFile(t, dir, "a.json", `{"host":"a"}`), Format: FormatJSON})
	mustAdd(t, c, Source[b]{Name: "app", Path: writeFile(t, dir, "b.json", `{"host":"b"}`), Format: FormatJSON})
	_, err := c.ParseFlags(nil)
	if err == nil || !strings.Contains(err.Error(), `flag --host is declared by sources "logs" and "app"`) {
		t.Fatalf("err = %v, want cross-source field flag collision", err)
	}
}

func TestParseFlagsDuplicateFieldFlagOnSameSource(t *testing.T) {
	type dup struct {
		A string `json:"a" flag:"level"`
		B string `json:"b" flag:"level"`
	}
	c := New()
	mustAdd(t, c, Source[dup]{Name: "app", Path: writeFile(t, t.TempDir(), "c.json", `{"a":"1","b":"2"}`), Format: FormatJSON})
	_, err := c.ParseFlags(nil)
	if err == nil || !strings.Contains(err.Error(), `flag --level is declared more than once on source "app"`) {
		t.Fatalf("err = %v, want same-source field flag collision", err)
	}
}

func TestSplitKnownFlags(t *testing.T) {
	known := map[string]reflect.Kind{
		"host": reflect.String,
		"log":  reflect.Bool,
	}
	cases := []struct {
		name      string
		args      []string
		wantParse []string
		wantRest  []string
	}{
		{"all known", []string{"--host", "x", "--log"}, []string{"--host", "x", "--log"}, nil},
		{"equals value", []string{"--host=x", "--log"}, []string{"--host=x", "--log"}, nil},
		{"bool then positional", []string{"--log", "serve"}, []string{"--log"}, []string{"serve"}},
		{"unknown then known", []string{"--migrate-db", "--host", "x"}, []string{"--host", "x"}, []string{"--migrate-db"}},
		{"interspersed", []string{"serve", "--host", "x", "get"}, []string{"--host", "x"}, []string{"serve", "get"}},
		{"positional first", []string{"serve", "--host", "x"}, []string{"--host", "x"}, []string{"serve"}},
		{"single dash", []string{"-h", "x"}, nil, []string{"-h", "x"}},
		{"terminator", []string{"--host", "x", "--", "--log"}, []string{"--host", "x"}, []string{"--log"}},
		{"terminator first", []string{"--", "--host", "x"}, nil, []string{"--host", "x"}},
		{"empty", nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parseList, rest := splitKnownFlags(tc.args, known)
			if !reflect.DeepEqual(parseList, tc.wantParse) {
				t.Fatalf("parseList = %v, want %v", parseList, tc.wantParse)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Fatalf("rest = %v, want %v", rest, tc.wantRest)
			}
		})
	}
}
