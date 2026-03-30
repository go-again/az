package az

import "errors"

var (
	// ErrInvalidMagic is returned when the stream does not start with the az magic bytes.
	ErrInvalidMagic = errors.New("az: invalid magic bytes")

	// ErrCorrupted is returned when the compressed data is malformed.
	ErrCorrupted = errors.New("az: corrupted data")

	// ErrChecksumFail is returned when a checksum does not match.
	ErrChecksumFail = errors.New("az: checksum mismatch")

	// ErrLevel is returned when an invalid compression level is provided.
	ErrLevel = errors.New("az: invalid compression level")

	// ErrTooBig is returned when a block exceeds the maximum supported size.
	ErrTooBig = errors.New("az: input too large")
)
