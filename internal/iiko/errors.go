package iiko

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidConfig           = errors.New("invalid iiko client configuration")
	ErrInvalidResponse         = errors.New("invalid iiko response")
	ErrPermissionDenied        = errors.New("iiko access denied")
	ErrUnauthorized            = errors.New("iiko authorization failed")
	ErrUnavailable             = errors.New("iiko temporarily unavailable")
	ErrValidation              = errors.New("iiko request rejected")
	ErrUnsupportedInterval     = errors.New("this OLAP adapter supports accounting-date reports, not intraday intervals")
	ErrMissingColumns          = errors.New("required OLAP columns unavailable")
	ErrOrganizationScope       = errors.New("OLAP requires an API key scoped to the configured organization only")
	ErrResultLimit             = errors.New("iiko result exceeds safety limit; narrow the period")
	ErrInvalidDiagnosticPeriod = errors.New("diagnostics require one completed calendar day")
)

type APIError struct {
	Operation     string
	Status        int
	CorrelationID string
	ResponseKind  string
	Cause         error
}

func (e *APIError) Error() string {
	message := e.Operation + ": " + e.Cause.Error()
	if e.Status > 0 {
		message = fmt.Sprintf("%s: HTTP %d: %s", e.Operation, e.Status, e.Cause)
	}
	if e.CorrelationID != "" {
		message += " (correlationId=" + e.CorrelationID + ")"
	}
	return message
}
func (e *APIError) Unwrap() error { return e.Cause }
