// Fixture: adapter-into-port (allowed)
package kernel

// Label takes an interface, like kernel.ErrorLabel.
func Label(err error) string { return err.Error() }
