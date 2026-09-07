package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed *.up.sql
var files embed.FS

func Up(ctx context.Context, db *sql.DB) error {
	// Keep the lock and all schema checks on the same transaction/connection.
	// Concurrent application starts may otherwise both try to apply a migration.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migrations: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		"SELECT pg_advisory_xact_lock(hashtextextended('kitchen:schema-migrations', 0))",
	); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version bigint PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	type migration struct {
		name    string
		version int64
	}
	ordered := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		versionText, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			return fmt.Errorf("invalid migration name %q", entry.Name())
		}
		version, err := strconv.ParseInt(versionText, 10, 64)
		if err != nil {
			return fmt.Errorf("parse migration version %q: %w", entry.Name(), err)
		}
		ordered = append(ordered, migration{name: entry.Name(), version: version})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].version < ordered[j].version })
	for index, migration := range ordered {
		version := migration.version
		if version <= 0 || (index > 0 && ordered[index-1].version == version) {
			return fmt.Errorf("invalid or duplicate migration version %d", version)
		}
		var applied bool
		if err := tx.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)", version,
		).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %d: %w", version, err)
		}
		if applied {
			continue
		}
		body, err := files.ReadFile(migration.name)
		if err != nil {
			return fmt.Errorf("read migration %q: %w", migration.name, err)
		}
		// The pgx driver accepts a whole script when there are no parameters.
		// Splitting on semicolons would corrupt SQL strings and function bodies.
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("apply migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO schema_migrations(version) VALUES ($1)", version,
		); err != nil {
			return fmt.Errorf("record migration %d: %w", version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}
