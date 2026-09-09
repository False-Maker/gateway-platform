// Package migrations embeds the ordered control-plane SQL files so tests and
// tooling apply the same schema the operator would.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed *.sql
var files embed.FS

// Execer is satisfied by pgx pools, connections, and transactions.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Files returns the migration file names in apply order.
func Files() ([]string, error) {
	entries, err := files.ReadDir(".")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Read returns the SQL of one migration file.
func Read(name string) (string, error) {
	data, err := files.ReadFile(name)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Apply executes every migration in order. All files are idempotent
// (CREATE ... IF NOT EXISTS), so re-applying is safe.
func Apply(ctx context.Context, exec Execer) error {
	names, err := Files()
	if err != nil {
		return err
	}
	for _, name := range names {
		sql, err := Read(name)
		if err != nil {
			return err
		}
		if _, err := exec.Exec(ctx, sql); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}
