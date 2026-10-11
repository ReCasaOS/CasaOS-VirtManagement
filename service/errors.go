package service

import "errors"

// The route maps these to status codes. Each is wrapped with the reason.
var (
	ErrInvalid      = errors.New("invalid request")
	ErrNotFound     = errors.New("virtual machine not found")
	ErrDiskNotFound = errors.New("disk not found")
	ErrConflict     = errors.New("conflict")
)
