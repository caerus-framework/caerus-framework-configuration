package cf_configuration

import (
	"os"
	"path/filepath"
	"testing"
)

type envSample struct {
	Host      string   `json:"host" env:"HOST"`
	Port      int      `json:"port" env:"PORT"`
	Addresses []string `json:"addresses" env:"ADDRESSES"`
	Name      string   `json:"client_name"` // derives CLIENT_NAME
}

func TestEnvOverlayOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`{"host":"file-host","port":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_HOST", "env-host")
	t.Setenv("APP_PORT", "99")

	c := New()
	if err := AddSource(c, Source[envSample]{
		Name:      "app",
		Path:      path,
		Format:    FormatJSON,
		EnvPrefix: "APP_",
	}); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	got := MustGet[envSample](c, "app")
	if got.Host != "env-host" || got.Port != 99 {
		t.Fatalf("got %+v, want env overlay to win", got)
	}
}

func TestEnvOnlySource(t *testing.T) {
	t.Setenv("APP_HOST", "only-env")
	t.Setenv("APP_ADDRESSES", "a:1, b:2")

	c := New()
	if err := AddSource(c, Source[envSample]{
		Name:      "app",
		EnvPrefix: "APP_",
	}); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	got := MustGet[envSample](c, "app")
	if got.Host != "only-env" {
		t.Fatalf("Host = %q", got.Host)
	}
	if len(got.Addresses) != 2 || got.Addresses[0] != "a:1" || got.Addresses[1] != "b:2" {
		t.Fatalf("Addresses = %#v", got.Addresses)
	}
}

func TestAfterLoadRunsAfterEnv(t *testing.T) {
	t.Setenv("APP_HOST", "from-env")
	c := New()
	if err := AddSource(c, Source[envSample]{
		Name:      "app",
		EnvPrefix: "APP_",
		AfterLoad: func(v *envSample) error {
			if v.Host != "from-env" {
				t.Fatalf("AfterLoad saw Host=%q before env applied", v.Host)
			}
			v.Host = "from-after"
			return nil
		},
	}); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if got := MustGet[envSample](c, "app").Host; got != "from-after" {
		t.Fatalf("Host = %q", got)
	}
}

func TestAddSourceRequiresPathOrEnvPrefix(t *testing.T) {
	c := New()
	err := AddSource(c, Source[envSample]{Name: "x", Format: FormatJSON})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDerivedEnvNameFromJSON(t *testing.T) {
	t.Setenv("APP_CLIENT_NAME", "cli")
	c := New()
	if err := AddSource(c, Source[envSample]{Name: "app", EnvPrefix: "APP_"}); err != nil {
		t.Fatal(err)
	}
	if got := MustGet[envSample](c, "app").Name; got != "cli" {
		t.Fatalf("Name = %q", got)
	}
}

func TestReloadPicksUpEnvWithUnchangedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`{"host":"file-host","port":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := New()
	if err := AddSource(c, Source[envSample]{
		Name:      "app",
		Path:      path,
		Format:    FormatJSON,
		EnvPrefix: "APP_",
	}); err != nil {
		t.Fatal(err)
	}
	if got := MustGet[envSample](c, "app").Host; got != "file-host" {
		t.Fatalf("Host = %q", got)
	}

	t.Setenv("APP_HOST", "env-later")
	if err := c.Reload("app"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := MustGet[envSample](c, "app").Host; got != "env-later" {
		t.Fatalf("after Reload Host = %q", got)
	}
}
