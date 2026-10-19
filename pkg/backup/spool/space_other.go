//go:build !linux

package spool

// freeBytes is not implemented off Linux, which the agent does not run on;
// the check is skipped so that the package still builds there for tests.
func freeBytes(string) (int64, bool, error) { return 0, false, nil }
