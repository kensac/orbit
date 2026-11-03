//go:build !linux

package runner

func systemMemoryPressure() bool { return false }
