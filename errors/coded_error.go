package errors

import (
	"errors"
	"fmt"
)

// CodedError is Aperture's canonical error type. Code identifies the failure
// class; Msg is the human-readable summary; Context holds structured detail;
// Inner is the wrapped cause. Use errors.Is / errors.As to inspect it.
type CodedError struct {
	Code    Code
	Msg     string
	Context map[string]any
	Inner   error
}

// Error implements the error interface.
func (e *CodedError) Error() string {
	if e.Inner != nil {
		return fmt.Sprintf("[%s] %s: %v", e.Code, e.Msg, e.Inner)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Msg)
}

// Unwrap exposes the wrapped error for errors.Is / errors.As.
func (e *CodedError) Unwrap() error { return e.Inner }

// New returns a fresh CodedError. When msg is empty it falls back to the code's
// canonical Registry message so every coded error carries a summary.
func New(code Code, msg string) *CodedError {
	if msg == "" {
		msg = Message(code)
	}
	return &CodedError{Code: code, Msg: msg}
}

// Newf is New with a formatted message.
func Newf(code Code, format string, args ...any) *CodedError {
	return &CodedError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// WithContext returns a fresh CodedError carrying structured context.
func WithContext(code Code, msg string, ctx map[string]any) *CodedError {
	if msg == "" {
		msg = Message(code)
	}
	return &CodedError{Code: code, Msg: msg, Context: ctx}
}

// Wrap attaches a code + summary to an existing error.
func Wrap(code Code, msg string, inner error) *CodedError {
	if msg == "" {
		msg = Message(code)
	}
	return &CodedError{Code: code, Msg: msg, Inner: inner}
}

// Wrapf is Wrap with a formatted message.
func Wrapf(code Code, inner error, format string, args ...any) *CodedError {
	return &CodedError{Code: code, Msg: fmt.Sprintf(format, args...), Inner: inner}
}

// CodeOf reports the Aperture Code for an error, or empty when none is attached.
func CodeOf(err error) Code {
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}
