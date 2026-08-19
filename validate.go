package cf_configuration

import (
	"errors"
	"fmt"
)

// FieldError attaches a configuration field name to a validation failure.
// Return it from Source.Validate so AddSource and reload errors name the
// field and constraint:
//
//	if cfg.MaxConns < 1 {
//	    return &FieldError{Field: "max_conns", Err: errors.New("must be >= 1")}
//	}
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Field == "" {
		return e.Err.Error()
	}
	return fmt.Sprintf("field %q: %s", e.Field, e.Err.Error())
}

func (e *FieldError) Unwrap() error { return e.Err }

func wrapValidateErr(err error) error {
	if err == nil {
		return nil
	}
	var fe *FieldError
	if errors.As(err, &fe) && fe != nil && fe.Field != "" {
		return fmt.Errorf("field %q: %w", fe.Field, fe.Err)
	}
	return err
}
