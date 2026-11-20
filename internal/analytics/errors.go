package analytics

import "errors"

var (
	ErrInvalidDecimal = errors.New("invalid decimal number")
	ErrDivisionByZero = errors.New("division by zero")
	ErrInvalidPeriod  = errors.New("invalid report period")
	ErrInvalidDataset = errors.New("invalid report dataset")
)
