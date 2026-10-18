//go:build linux || darwin

package local

func unsupportedPlatform() error { return nil }
