//go:build !darwin

package main

import "errors"

// The harness drives a macOS test machine; elsewhere the VM check cannot be
// answered, so it refuses. This file exists so the package builds, vets and
// tests on Linux CI.
func hostIsVM() (bool, error) {
	return false, errors.New("the VM check runs on macOS only")
}
