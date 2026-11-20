package supervisor

import "errors"

var (
	ErrConfiguration   = errors.New("invalid supervisor configuration")
	ErrShutdownTimeout = errors.New("supervised process did not stop within the shutdown timeout")
)
