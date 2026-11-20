package assistant

import (
	"errors"
	"fmt"
)

var (
	ErrConfiguration   = errors.New("assistant configuration is incomplete")
	ErrUnavailable     = errors.New("OpenAI service is unavailable")
	ErrToolLimit       = errors.New("assistant tool budget exceeded")
	ErrInvalidTool     = errors.New("unsupported assistant tool or arguments")
	ErrInvalidInsight  = errors.New("assistant returned an unsupported interpretation")
	ErrPhotoTooLarge   = errors.New("photo exceeds byte or pixel limit")
	ErrPhotoFormat     = errors.New("only JPEG and PNG photos are supported")
	ErrInvalidPhoto    = errors.New("photo header is invalid")
	ErrPhotoUnreadable = errors.New("photo text could not be read")
	ErrPhotoResponse   = errors.New("photo recognition returned an invalid or incomplete response")
)

// APIError exposes the HTTP status without including response bodies, images,
// credentials, or provider error text in user-facing diagnostics.
type APIError struct{ Status int }

func (e *APIError) Error() string { return fmt.Sprintf("OpenAI HTTP %d: unavailable", e.Status) }
func (e *APIError) Unwrap() error { return ErrUnavailable }
