package cf_configuration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cf "github.com/caerus-framework/caerus-framework"
	cf_logs "github.com/caerus-framework/caerus-framework-logs"
)

type mongoConfig struct {
	Host string `json:"host" yaml:"host"`
	Port int    `json:"port" yaml:"port"`
}

func TestAddSourceValueGenericFree(t *testing.T) {
	t.Setenv("APP_HOST", "env-host")
	c := New()
	path := writeFile(t, t.TempDir(), "obs.json", `{"port":27018}`)

	if err := c.AddSourceValue(cf.ConfigSourceValue{
		Name:      "observability",
		Path:      path,
		Format:    "json",
		EnvPrefix: "APP_",
		Owner:     "observability",
		Sample:    mongoConfig{},
	}); err != nil {
		t.Fatalf("AddSourceValue: %v", err)
	}
	got, ok := Get[mongoConfig](c, "observability")
	if !ok {
		t.Fatal("source not registered with the sample's dynamic type")
	}
	if got.Host != "env-host" || got.Port != 27018 {
		t.Fatalf("got %+v, want file value + env overlay", got)
	}

	if err := c.AddSourceValue(cf.ConfigSourceValue{Name: "x", Sample: mongoConfig{}}); err == nil {
		t.Fatal("Path and/or EnvPrefix validation should fail")
	}
	if err := c.AddSourceValue(cf.ConfigSourceValue{Name: "y", Path: "z.json", Sample: nil}); err == nil {
		t.Fatal("nil Sample should fail")
	}
	if err := c.AddSourceValue(cf.ConfigSourceValue{Name: "z", Path: path, Format: "toml", Sample: mongoConfig{}}); err == nil {
		t.Fatal("unknown format should fail")
	}
}

func TestAddSourceValueGenericFreeAfterLoadAndValidate(t *testing.T) {
	c := New()
	path := writeFile(t, t.TempDir(), "obs.json", `{"host":"file-host","port":27018}`)

	if err := c.AddSourceValue(cf.ConfigSourceValue{
		Name:   "observability",
		Path:   path,
		Format: "json",
		Sample: mongoConfig{},
		AfterLoad: func(v any) error {
			cfg, ok := v.(*mongoConfig)
			if !ok {
				t.Fatalf("AfterLoad got %T, want *mongoConfig", v)
			}
			cfg.Host = "after-" + cfg.Host
			return nil
		},
		Validate: func(v any) error {
			cfg, ok := v.(*mongoConfig)
			if !ok {
				t.Fatalf("Validate got %T, want *mongoConfig", v)
			}
			if cfg.Port == 0 {
				return errors.New("port is required")
			}
			if cfg.Host != "after-file-host" {
				return errors.New("AfterLoad must run before Validate")
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("AddSourceValue: %v", err)
	}

	got, ok := Get[mongoConfig](c, "observability")
	if !ok {
		t.Fatal("source not registered with generic-free hooks")
	}
	if got.Host != "after-file-host" || got.Port != 27018 {
		t.Fatalf("got %+v, want AfterLoad result + validated value", got)
	}
}

func TestAddSourceValueGenericFreeValidateRejects(t *testing.T) {
	c := New()
	path := writeFile(t, t.TempDir(), "obs.json", `{"host":"file-host","port":0}`)

	err := c.AddSourceValue(cf.ConfigSourceValue{
		Name:   "observability",
		Path:   path,
		Format: "json",
		Sample: mongoConfig{},
		Validate: func(v any) error {
			cfg, ok := v.(*mongoConfig)
			if !ok {
				t.Fatalf("Validate got %T, want *mongoConfig", v)
			}
			if cfg.Port == 0 {
				return errors.New("port is required")
			}
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "port is required") {
		t.Fatalf("expected validation error, got %v", err)
	}
	if _, ok := Get[mongoConfig](c, "observability"); ok {
		t.Fatal("failed generic-free source must not be registered")
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func mustAddSource(t *testing.T, c *Configuration, src Source[mongoConfig]) {
	t.Helper()
	if err := AddSource(c, src); err != nil {
		t.Fatalf("AddSource(%q): %v", src.Name, err)
	}
}

// eventually polls cond until it returns true or the deadline passes.
func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, msg)
}

func TestAddSourceAndGet(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "mongo.json", `{"host":"localhost","port":27017}`)

	c := New()
	mustAddSource(t, c, Source[mongoConfig]{Name: "mongo", Path: path, Format: FormatJSON})

	got, ok := Get[mongoConfig](c, "mongo")
	if !ok {
		t.Fatal("Get returned false for a registered source")
	}
	if got.Host != "localhost" || got.Port != 27017 {
		t.Fatalf("unexpected config: %+v", got)
	}
	if v := MustGet[mongoConfig](c, "mongo"); v != got {
		t.Fatal("MustGet returned a different value")
	}
	looked, err := Lookup[mongoConfig](c, "mongo")
	if err != nil || looked != got {
		t.Fatalf("Lookup = %v, %v; want same value as Get", looked, err)
	}
}

func TestLookupMissing(t *testing.T) {
	c := New()
	_, err := Lookup[mongoConfig](c, "missing")
	if err == nil {
		t.Fatal("Lookup missing source should error")
	}
	if !strings.Contains(err.Error(), "no configuration source") {
		t.Fatalf("err = %v, want no configuration source", err)
	}
}

func TestMustGetPanicsOnMissing(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustGet should panic for missing source")
		}
	}()
	MustGet[mongoConfig](New(), "missing")
}

func TestYAMLSource(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "mongo.yaml", "host: localhost\nport: 27017\n")

	c := New()
	mustAddSource(t, c, Source[mongoConfig]{Name: "mongo", Path: path, Format: FormatYAML})

	got, ok := Get[mongoConfig](c, "mongo")
	if !ok || got.Port != 27017 || got.Host != "localhost" {
		t.Fatalf("unexpected yaml config: %+v, %v", got, ok)
	}
}

func TestAddSourceFailFast(t *testing.T) {
	dir := t.TempDir()

	c := New()
	// Missing file.
	err := AddSource(c, Source[mongoConfig]{Name: "missing", Path: filepath.Join(dir, "nope.json"), Format: FormatJSON})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected error for missing file, got %v", err)
	}
	if _, ok := Get[mongoConfig](c, "missing"); ok {
		t.Fatal("failed source must not be registered")
	}

	// Malformed content.
	bad := writeFile(t, dir, "bad.json", `{not json`)
	if err := AddSource(c, Source[mongoConfig]{Name: "bad", Path: bad, Format: FormatJSON}); err == nil {
		t.Fatal("expected error for malformed config")
	}
	if _, ok := Get[mongoConfig](c, "bad"); ok {
		t.Fatal("failed source must not be registered")
	}

	// Rejected by the validator.
	reject := writeFile(t, dir, "reject.json", `{"host":"localhost","port":0}`)
	err = AddSource(c, Source[mongoConfig]{
		Name: "reject", Path: reject, Format: FormatJSON,
		Validate: func(v *mongoConfig) error {
			if v.Port == 0 {
				return errors.New("port is required")
			}
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "port") {
		t.Fatalf("expected validation error, got %v", err)
	}
	if _, ok := Get[mongoConfig](c, "reject"); ok {
		t.Fatal("failed source must not be registered")
	}
}

func TestGetWrongTypeAndUnknown(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "mongo.json", `{"host":"localhost","port":27017}`)

	c := New()
	mustAddSource(t, c, Source[mongoConfig]{Name: "mongo", Path: path, Format: FormatJSON})

	if _, ok := Get[string](c, "mongo"); ok {
		t.Fatal("Get[string] must not match a mongoConfig source")
	}
	if _, ok := Get[mongoConfig](c, "unknown"); ok {
		t.Fatal("Get on an unknown name must report false")
	}
}

func TestDuplicateSourceRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "mongo.json", `{"host":"a","port":1}`)

	c := New()
	mustAddSource(t, c, Source[mongoConfig]{Name: "mongo", Path: path, Format: FormatJSON})
	if err := AddSource(c, Source[mongoConfig]{Name: "mongo", Path: path, Format: FormatJSON}); err == nil {
		t.Fatal("expected error for duplicate source name")
	}
}

func TestAddSourceValidation(t *testing.T) {
	c := New()
	if err := AddSource(c, Source[mongoConfig]{Path: "/x", Format: FormatJSON}); err == nil {
		t.Fatal("expected error for empty name")
	}
	if err := AddSource(c, Source[mongoConfig]{Name: "x", Format: FormatJSON}); err == nil {
		t.Fatal("expected error for empty Path and EnvPrefix")
	}
	if err := AddSource(c, Source[mongoConfig]{Name: "x", Path: "/x", Format: Format(99)}); err == nil {
		t.Fatal("expected error for unknown format")
	}
}

// fakeComp is a minimal cf.CaerusComponent for tests.
type fakeComp struct {
	name  string
	stage cf.Stage
}

func newFake(name string, stage cf.Stage) *fakeComp {
	return &fakeComp{name: name, stage: stage}
}

func (f *fakeComp) Name() string                { return f.name }
func (f *fakeComp) GetInitOrderStage() cf.Stage { return f.stage }
func (f *fakeComp) Init(ctx context.Context, fw *cf.CaerusFramework) error {
	return nil
}
func (f *fakeComp) Shutdown(ctx context.Context) error { return nil }

// reloadHook records invocations of the source owner's OnConfigReload.
type reloadHook struct {
	*fakeComp
	reloads atomic.Int64
}

func (r *reloadHook) OnConfigReload(source string, cfg any) { r.reloads.Add(1) }

// newFramework returns a framework with the logs component registered, which
// the configuration component depends on.
func newFramework(t *testing.T) *cf.CaerusFramework {
	t.Helper()
	fw := cf.New()
	if err := fw.AddComponent(cf_logs.New(cf_logs.WithWriter(io.Discard))); err != nil {
		t.Fatalf("AddComponent(logs): %v", err)
	}
	return fw
}

func TestReloadOnFileChange(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "mongo.json", `{"host":"localhost","port":27017}`)

	fw := newFramework(t)
	owner := &reloadHook{fakeComp: newFake("mongocomp", cf.ConfigurationStage)}
	if err := fw.AddComponent(owner); err != nil {
		t.Fatalf("AddComponent: %v", err)
	}

	c := New(WithLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))))
	if err := fw.AddComponent(c); err != nil {
		t.Fatalf("AddComponent(config): %v", err)
	}
	mustAddSource(t, c, Source[mongoConfig]{Name: "mongo", Path: path, Format: FormatJSON, Owner: "mongocomp"})

	if err := fw.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"host":"db.internal","port":27018}`), 0o644); err != nil {
		t.Fatalf("update file: %v", err)
	}

	eventually(t, 3*time.Second, func() bool {
		got, ok := Get[mongoConfig](c, "mongo")
		return ok && got.Port == 27018
	}, "config to reload to the new value")

	if owner.reloads.Load() == 0 {
		t.Fatal("expected the owner's OnConfigReload to be invoked")
	}
	if err := fw.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestReloadNoChangeSkipsNotify(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "mongo.json", `{"host":"localhost","port":27017}`)
	fw := newFramework(t)
	owner := &reloadHookWithGet{fakeComp: newFake("mongocomp", cf.ConfigurationStage)}
	if err := fw.AddComponent(owner); err != nil {
		t.Fatalf("AddComponent: %v", err)
	}

	c := New(WithLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))))
	if err := fw.AddComponent(c); err != nil {
		t.Fatalf("AddComponent(config): %v", err)
	}
	owner.cfg = c
	mustAddSource(t, c, Source[mongoConfig]{Name: "mongo", Path: path, Format: FormatJSON, Owner: "mongocomp"})
	if err := fw.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// Initialize delivers the initial value to each source's owner once (core
	// components boot on defaults and receive their configuration via this
	// notify). Identical content must not add a change-notify.
	if n := owner.reloads.Load(); n != 1 {
		t.Fatalf("expected one initial notify on Initialize, got %d", n)
	}

	// Rewrite identical content (e.g. a K8s symlink swap): must not reload.
	if err := os.WriteFile(path, []byte(`{"host":"localhost","port":27017}`), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := owner.reloads.Load(); n != 1 {
		t.Fatalf("expected no reload on identical content, got %d", n)
	}
	if err := fw.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestInvalidReloadKeepsPreviousValue(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "mongo.json", `{"host":"localhost","port":27017}`)

	c := New()
	mustAddSource(t, c, Source[mongoConfig]{
		Name: "mongo", Path: path, Format: FormatJSON,
		Validate: func(v *mongoConfig) error {
			if v.Port <= 0 || v.Port > 65535 {
				return errors.New("invalid port")
			}
			return nil
		},
	})
	if err := c.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer c.Shutdown(context.Background())

	// Malformed JSON: reload rejected, old value stays.
	if err := os.WriteFile(path, []byte(`{broken`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	got, ok := Get[mongoConfig](c, "mongo")
	if !ok || got.Port != 27017 {
		t.Fatalf("expected previous value after invalid reload, got %+v (ok=%v)", got, ok)
	}

	// Valid JSON but rejected by the validator: old value stays.
	if err := os.WriteFile(path, []byte(`{"host":"x","port":99999}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	got, ok = Get[mongoConfig](c, "mongo")
	if !ok || got.Port != 27017 {
		t.Fatalf("expected previous value after rejected validation, got %+v (ok=%v)", got, ok)
	}
}

// TestReloadCallbackCanCallGet verifies that OnConfigReload callbacks can
// safely call Get/MustGet without deadlocking. This is a regression test for
// the deadlock fix where handleEvent held c.mu while invoking the callback.
func TestReloadCallbackCanCallGet(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "mongo.json", `{"host":"localhost","port":27017}`)

	fw := newFramework(t)
	owner := &reloadHookWithGet{fakeComp: newFake("mongocomp", cf.ConfigurationStage)}
	if err := fw.AddComponent(owner); err != nil {
		t.Fatalf("AddComponent: %v", err)
	}

	c := New(WithLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))))
	if err := fw.AddComponent(c); err != nil {
		t.Fatalf("AddComponent(config): %v", err)
	}
	owner.cfg = c
	mustAddSource(t, c, Source[mongoConfig]{Name: "mongo", Path: path, Format: FormatJSON, Owner: "mongocomp"})

	if err := fw.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// Trigger a reload. The owner's OnConfigReload will call MustGet, which
	// must not deadlock.
	if err := os.WriteFile(path, []byte(`{"host":"db.internal","port":27018}`), 0o644); err != nil {
		t.Fatalf("update file: %v", err)
	}

	// The change-notify must read the new value. The initial notify on
	// Initialize already delivered the 27017 value, so wait until the change
	// notify has run (a deadlock would time this out).
	eventually(t, 3*time.Second, func() bool {
		lastValue := owner.lastValue.Load()
		return lastValue != nil && lastValue.Port == 27018
	}, "owner's OnConfigReload to read the new value")

	if n := owner.reloads.Load(); n < 2 {
		t.Fatalf("expected initial + change notify, got %d", n)
	}

	if err := fw.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// reloadHookWithGet is a test owner that calls MustGet from OnConfigReload.
type reloadHookWithGet struct {
	*fakeComp
	reloads   atomic.Int64
	lastValue atomic.Pointer[mongoConfig]
	cfg       *Configuration
}

func (r *reloadHookWithGet) OnConfigReload(source string, cfg any) {
	r.reloads.Add(1)
	// This call would deadlock before the fix.
	v := MustGet[mongoConfig](r.cfg, "mongo")
	r.lastValue.Store(&v)
}

func TestAddSourceAfterInit(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "mongo.json", `{"host":"localhost","port":27017}`)

	c := New()
	if err := c.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer c.Shutdown(context.Background())

	// Sources added after Init are loaded and watched immediately.
	mustAddSource(t, c, Source[mongoConfig]{Name: "mongo", Path: path, Format: FormatJSON})

	if err := os.WriteFile(path, []byte(`{"host":"db","port":27018}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	eventually(t, 3*time.Second, func() bool {
		got, ok := Get[mongoConfig](c, "mongo")
		return ok && got.Port == 27018
	}, "config added after Init to reload")
}

func TestComponentContract(t *testing.T) {
	c := New()
	if c.Name() != ComponentName {
		t.Fatalf("Name() = %q", c.Name())
	}
	if c.GetInitOrderStage() != cf.ConfigurationStage {
		t.Fatalf("GetInitOrderStage() = %q", c.GetInitOrderStage())
	}
	if deps := c.GetDependencies(); len(deps) != 1 || deps[0] != cf_logs.ComponentName {
		t.Fatalf("GetDependencies() = %v, want [logs]", deps)
	}
	// Shutdown before Init must be a no-op, not a panic.
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Init: %v", err)
	}
}

func TestInitUsesFrameworkLogger(t *testing.T) {
	logs := cf_logs.New(cf_logs.WithWriter(io.Discard))
	fw := cf.New()
	if err := fw.AddComponent(logs); err != nil {
		t.Fatalf("AddComponent(logs): %v", err)
	}

	c := New()
	if err := c.Init(context.Background(), fw); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer c.Shutdown(context.Background())
	if c.logger == nil || c.logsSub == nil {
		t.Fatal("Init should subscribe to the framework logs component")
	}
	before := c.logger
	if before == logs.Logger() {
		t.Fatal("component logger must be OnReconfigureFor-scoped, not the process-global Logger()")
	}

	logs.Reconfigure(cf_logs.WithWriter(io.Discard))
	if c.logger == before {
		t.Fatal("component should receive the rebuilt logger on Reconfigure")
	}
	if c.logger == logs.Logger() {
		t.Fatal("rebuilt logger must remain OnReconfigureFor-scoped")
	}

	// an explicit WithLogger wins over the framework logger
	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	c2 := New(WithLogger(custom))
	if err := c2.Init(context.Background(), fw); err != nil {
		t.Fatalf("Init (with logger): %v", err)
	}
	defer c2.Shutdown(context.Background())
	if c2.logger != custom {
		t.Fatal("explicit WithLogger should win over the framework logger")
	}
}

func TestFrameworkIntegration(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "mongo.json", `{"host":"localhost","port":27017}`)

	fw := cf.New() // LogsStage + ConfigurationStage are built-in bootstrap stages
	logs := cf_logs.New(cf_logs.WithWriter(io.Discard))
	if err := fw.AddComponent(logs); err != nil {
		t.Fatalf("AddComponent(logs): %v", err)
	}
	c := New()
	if err := fw.AddComponent(c); err != nil {
		t.Fatalf("AddComponent: %v", err)
	}
	mustAddSource(t, c, Source[mongoConfig]{Name: "mongo", Path: path, Format: FormatJSON})

	order, err := fw.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(order) != 2 || order[0] != logs || order[1] != c {
		t.Fatalf("unexpected order %v", order)
	}

	if err := fw.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, ok := cf.Get[*Configuration](fw)
	if !ok || got != c {
		t.Fatal("Get[*Configuration] did not return the component")
	}
}

func TestAddSourceRejectsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(MaxConfigFileBytes)+1), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := New()
	err := AddSource(c, Source[mongoConfig]{Name: "mongo", Path: path, Format: FormatJSON})
	if err == nil {
		t.Fatal("AddSource must reject a file larger than MaxConfigFileBytes")
	}
	if !strings.Contains(err.Error(), "max") && !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("error should mention the size cap, got %v", err)
	}
}

func TestAddSourceAcceptsFileAtMaxSize(t *testing.T) {
	dir := t.TempDir()
	body := []byte(`{"host":"x","port":1}`)
	data := make([]byte, MaxConfigFileBytes)
	copy(data, body)
	for i := len(body); i < len(data); i++ {
		data[i] = ' '
	}
	path := filepath.Join(dir, "mongo.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := New()
	mustAddSource(t, c, Source[mongoConfig]{Name: "mongo", Path: path, Format: FormatJSON})
	got, ok := Get[mongoConfig](c, "mongo")
	if !ok || got.Host != "x" || got.Port != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestOversizedReloadKeepsPreviousValue(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "mongo.json", `{"host":"localhost","port":27017}`)
	c := New()
	mustAddSource(t, c, Source[mongoConfig]{Name: "mongo", Path: path, Format: FormatJSON})
	if err := c.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })

	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(MaxConfigFileBytes)+1), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := c.Reload("mongo")
	if err == nil {
		t.Fatal("Reload of an oversized file must fail")
	}
	got, ok := Get[mongoConfig](c, "mongo")
	if !ok || got.Port != 27017 {
		t.Fatalf("expected previous value after oversized reload, got %+v (ok=%v)", got, ok)
	}
}

var _ cf.CaerusComponent = (*Configuration)(nil)
