package state

import "errors"

var (
	ErrInvalid        = errors.New("invalid technical state")
	ErrAlreadyRunning = errors.New("another instance may be running; inspect run/state.json.lock before removing a stale lock")
)
