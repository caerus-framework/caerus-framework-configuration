package cf_configuration

import (
	"errors"
	"strings"
	"testing"
)

func TestFieldErrorMessage(t *testing.T) {
	err := &FieldError{Field: "max_conns", Err: errors.New("must be >= 1")}
	if got := err.Error(); got != `field "max_conns": must be >= 1` {
		t.Fatalf("Error() = %q", got)
	}
}

func TestAddSourceWrapsFieldError(t *testing.T) {
	c := New()
	dir := t.TempDir()
	path := writeFile(t, dir, "bad.json", `{"host":"h"}`)

	err := AddSource(c, Source[mongoConfig]{
		Name:   "mongo",
		Path:   path,
		Format: FormatJSON,
		Validate: func(v *mongoConfig) error {
			return &FieldError{Field: "host", Err: errors.New("must not be h")}
		},
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	if !strings.Contains(msg, `source "mongo"`) {
		t.Fatalf("error = %q, want source name", msg)
	}
	if !strings.Contains(msg, `field "host"`) {
		t.Fatalf("error = %q, want field name", msg)
	}
	if !strings.Contains(msg, "must not be h") {
		t.Fatalf("error = %q, want constraint", msg)
	}
}

func TestAddSourcePlainValidateErrorUnchanged(t *testing.T) {
	c := New()
	dir := t.TempDir()
	path := writeFile(t, dir, "bad.json", `{}`)

	err := AddSource(c, Source[mongoConfig]{
		Name:   "mongo",
		Path:   path,
		Format: FormatJSON,
		Validate: func(v *mongoConfig) error {
			return errors.New("bad config")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "bad config") {
		t.Fatalf("error = %v, want plain validate message preserved", err)
	}
}
