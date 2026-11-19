//go:build compat && e2e

package e2e

// SetAuditRowLimit lowers the audit row limit for a test and returns the function that restores it.
func SetAuditRowLimit(n int) (restore func()) {
	old := auditRowLimit
	auditRowLimit = n
	return func() { auditRowLimit = old }
}
