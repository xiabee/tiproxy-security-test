// Copyright 2023 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package errors

import (
	"errors"
	"fmt"
	"runtime"
)

const defaultStackDepth = 48

var (
	_ error         = &Error{}
	_ fmt.Formatter = &Error{}
)

// Error is a simple error wrapper with stacktrace.
type Error struct {
	err   error
	trace stacktrace
}

// WithStack will wrapping an error with stacktrace, given a default stack depth.
func WithStack(err error) error {
	if err == nil {
		return nil
	}
	e := &Error{err: err}
	e.withStackDepth(1, defaultStackDepth)
	return e
}

// WithStackDepth is like WithStack, but can specify stack depth.
func WithStackDepth(err error, depth int) error {
	e := &Error{err: err}
	e.withStackDepth(1, depth)
	return e
}

func (e *Error) withStackDepth(skip, depth int) {
	e.trace = make(stacktrace, depth)
	runtime.Callers(2+skip, e.trace)
}

// Format implements `fmt.Formatter`. %+v/%v will contain stacktrace compared to %s.
func (e *Error) Format(st fmt.State, verb rune) {
	switch verb {
	case 'v':
		if st.Flag('+') {
			fmt.Fprintf(st, "%+v", e.err)
			e.trace.Format(st, 'v')
		} else {
			fmt.Fprintf(st, "%v", e.err)
			e.trace.Format(st, 'v')
		}
	case 's':
		if st.Flag('+') {
			fmt.Fprintf(st, "%+s", e.err)
			e.trace.Format(st, 's')
		} else {
			fmt.Fprintf(st, "%s", e.err)
		}
	}
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s", e)
}

func (e *Error) Is(target error) bool {
	return errors.Is(e.err, target)
}

func (e *Error) As(target interface{}) bool {
	return errors.As(e.err, target)
}

func (e *Error) Unwrap() error {
	return errors.Unwrap(e.err)
}

type Warning struct {
	Err error
}

func (e *Warning) Error() string {
	return e.Err.Error()
}

func (e *Warning) Unwrap() error {
	return e.Err
}

func (e *Warning) Is(target error) bool {
	_, ok := target.(*Warning)
	return ok
}
