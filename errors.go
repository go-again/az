package az

import "errors"

var (
	// ErrCorrupted is returned when the compressed data is malformed.
	ErrCorrupted = errors.New("az: corrupted data")

	// ErrChecksumFail is returned when a checksum does not match.
	ErrChecksumFail = errors.New("az: checksum mismatch")

	// ErrLevel is returned when an invalid compression level is provided.
	ErrLevel = errors.New("az: invalid compression level")

	// ErrTooLarge is returned by Decoder.DecodeAllLimit when the decompressed
	// output would exceed the caller's limit. It is distinct from ErrCorrupted so
	// callers can tell a decompression bomb from a malformed frame.
	ErrTooLarge = errors.New("az: decompressed size exceeds limit")
)
