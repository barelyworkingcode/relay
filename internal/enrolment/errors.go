package enrolment

import "errors"

var (
	ErrNotFound = errors.New("enrolment not found")
	ErrInvalid  = errors.New("invalid enrolment")
	// ErrBundle means the enrolment record COMMITTED to settings
	// and only the on-disk bundle (client key, client cert, CA cert) failed
	// to write. Callers must not treat it as a failed creation: the
	// returned enrolment is the record that landed, and reporting it as a
	// plain error would lose a credential the operator can already see in
	// settings.json but has no bundle to hand to the client.
	ErrBundle = errors.New("enrolment bundle")
)

// Carries the reason text verbatim, the same trick serviceValidationError
// uses: wrapping with %w would prefix the sentinel's own text, and this
// package's messages already name the offending grant or client id for the
// operator to act on as-is.
type validationError struct{ reason string }

func (e *validationError) Error() string        { return e.reason }
func (e *validationError) Is(target error) bool { return target == ErrInvalid }

// Invalid wraps a reason as an ErrInvalid. Exported because the gated core
// in package main spells its own argument refusals with it, and a second
// constructor there would produce an error that errors.Is(ErrInvalid)
// reports false for.
func Invalid(reason string) error {
	return &validationError{reason: reason}
}
