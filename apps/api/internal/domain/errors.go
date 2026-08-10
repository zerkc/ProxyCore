package domain

// ValidationError mirrors the Node DomainValidationError: a validation failure
// that HTTP handlers surface as a 400 with an {error} body.
type ValidationError struct {
	Message string
	Code    string
}

func (e *ValidationError) Error() string { return e.Message }

func newValidationError(message, code string) *ValidationError {
	if code == "" {
		code = "DOMAIN_VALIDATION"
	}
	return &ValidationError{Message: message, Code: code}
}

func assertDomain(condition bool, message, code string) {
	if !condition {
		panic(newValidationError(message, code))
	}
}

// Recover converts a panic raised by assertDomain into an error. Validators use
// panic/recover internally to keep the ported control flow close to the Node
// source; the public entry points recover and return the error.
func recoverValidation(errp *error) {
	if r := recover(); r != nil {
		if ve, ok := r.(*ValidationError); ok {
			*errp = ve
			return
		}
		panic(r)
	}
}
