//go:build !linux && !darwin

package app

// discardPendingInput does nothing where the project has no terminal flush (it ships for Linux
// and macOS).
func discardPendingInput(int) {}
