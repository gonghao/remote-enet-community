package integration_test

import (
	"context"
	"testing"
)

type fixtureT interface {
	Context() context.Context
	Helper()
	Fatal(...any)
	Fatalf(string, ...any)
	Error(...any)
	Errorf(string, ...any)
	Logf(string, ...any)
	Cleanup(func())
}

type fixtureScope struct {
	*testing.T
	ctx      context.Context
	cleanups []func()
	logs     uint64
}

func (s *fixtureScope) Context() context.Context { return s.ctx }
func (s *fixtureScope) Cleanup(f func())         { s.cleanups = append(s.cleanups, f) }

// Repeated success logs are counted rather than retained by testing.T for hours.
// Failures still use the real test's Fatal/Error methods and diagnostics.
func (s *fixtureScope) Logf(string, ...any) { s.logs++ }

func (s *fixtureScope) close() {
	for _, cleanup := range s.cleanups {
		defer cleanup()
	}
	s.cleanups = nil
}
