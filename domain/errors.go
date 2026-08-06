package domain

import "errors"

var (
	ErrNotFound            = errors.New("not found")
	ErrConflict            = errors.New("conflict")
	ErrInvalidArgument     = errors.New("invalid argument")
	ErrForbidden           = errors.New("forbidden")
	ErrInsufficientStorage = errors.New("insufficient storage")
	// ErrUnsupportedFS is returned when versioning is disabled. The historic
	// name remains part of the public error contract; enabled versioning can use
	// byte copies on filesystems without reflink support.
	ErrUnsupportedFS = errors.New("unsupported filesystem for versioning")
)
