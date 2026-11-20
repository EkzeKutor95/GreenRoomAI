package bot

import "errors"

var (
	ErrConfig           = errors.New("invalid bot configuration or dependencies")
	ErrSelection        = errors.New("invalid or expired menu selection")
	ErrPeriod           = errors.New("report period must not exceed 366 calendar days")
	ErrPhotoMetadata    = errors.New("invalid photo metadata")
	ErrPhotoDimensions  = errors.New("photo exceeds pixel limit")
	ErrPhotoFileType    = errors.New("unsupported photo document format")
	ErrPhotoEmptyResult = errors.New("photo recognition returned no text")
)
