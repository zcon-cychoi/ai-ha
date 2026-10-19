package rest

import (
	"fmt"

	"github.com/go-playground/validator/v10"
)

// requestValidator adapts go-playground/validator to the echo.Validator
// interface so that handlers can call c.Validate on bound request structs.
type requestValidator struct {
	validate *validator.Validate
}

// newValidator returns the echo validator used by the REST API server.
func newValidator() *requestValidator {
	return &requestValidator{validate: validator.New(validator.WithRequiredStructEnabled())}
}

// Validate reports whether i satisfies its validate struct tags.
func (v *requestValidator) Validate(i any) error {
	if err := v.validate.Struct(i); err != nil {
		return fmt.Errorf("request validation failed: %w", err)
	}
	return nil
}
