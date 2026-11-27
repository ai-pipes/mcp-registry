//go:build compat

package harness

// AppContainer is the name of the stack's app container, for a scenario that measures it from the
// outside (docker stats). It is read-only: nothing in the suite changes the container by it.
func (s *Stack) AppContainer() string { return s.app }
