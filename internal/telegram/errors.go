package telegram

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrConfig       = errors.New("invalid Telegram client configuration")
	ErrTransport    = errors.New("Telegram transport failure")
	ErrResponse     = errors.New("invalid Telegram response")
	ErrRequest      = errors.New("Telegram rejected request")
	ErrFileTooLarge = errors.New("Telegram file exceeds the 10 MiB download limit")
	ErrFilePath     = errors.New("Telegram returned an unsafe file path")
	ErrFileID       = errors.New("invalid Telegram file identifier")
)

// APIError intentionally excludes Telegram response text and the token-bearing URL.
type APIError struct {
	Method     string
	Code       int
	RetryAfter time.Duration
}

func (e *APIError) Error() string { return fmt.Sprintf("Telegram %s: status %d", e.Method, e.Code) }
func (e *APIError) Unwrap() error { return ErrRequest }
