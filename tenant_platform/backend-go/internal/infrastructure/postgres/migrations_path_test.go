package postgres

import (
	"path/filepath"
	"testing"
)

func TestMigrationsDirHonorsExplicitEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GA_MIGRATIONS_DIR", dir)

	if got, want := migrationsDir(), filepath.Clean(dir); got != want {
		t.Fatalf("migrationsDir() = %q, want explicit directory %q", got, want)
	}
}
