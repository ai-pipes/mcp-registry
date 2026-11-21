package infra

// PgRepo is an adapter.
type PgRepo struct{}

// Get implements app.Repo.
func (*PgRepo) Get() string { return "x" }
