// Package database applies the repository's versioned PostgreSQL schema.
// PostgreSQL's docker-entrypoint init directory is deliberately not used: it
// only runs for a brand-new volume and silently skips upgrades.
package database

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

var migrationFilename = regexp.MustCompile(`^(\d{4,})_[A-Za-z0-9_-]+\.sql$`)

type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum [32]byte
}

func LoadMigrations(directory string) ([]Migration, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read migrations directory: %w", err)
	}
	migrations := make([]Migration, 0, len(entries))
	versions := make(map[int]struct{})
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := migrationFilename.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		version, err := strconv.Atoi(match[1])
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("invalid migration version in %q", entry.Name())
		}
		if _, exists := versions[version]; exists {
			return nil, fmt.Errorf("duplicate migration version %d", version)
		}
		contents, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		migrations = append(migrations, Migration{Version: version, Name: entry.Name(), SQL: string(contents), Checksum: sha256.Sum256(contents)})
		versions[version] = struct{}{}
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	if len(migrations) == 0 {
		return nil, errors.New("no migration files found")
	}
	return migrations, nil
}

// Apply installs a checksum-protected schema_migrations ledger, serializes all
// runners with a PostgreSQL advisory lock, and applies every migration in one
// database transaction. Existing installations from the pre-ledger era can be
// explicitly baselined only by an operator who supplies baselineVersion.
func Apply(ctx context.Context, conn *pgx.Conn, migrations []Migration, baselineVersion int) error {
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(70420260824)`); err != nil {
		return fmt.Errorf("lock migration runner: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(70420260824)`) }()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version integer PRIMARY KEY,
			name text NOT NULL,
			checksum bytea NOT NULL CHECK (octet_length(checksum) = 32),
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema migration ledger: %w", err)
	}

	var appliedCount int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&appliedCount); err != nil {
		return fmt.Errorf("count schema migrations: %w", err)
	}
	if appliedCount == 0 && baselineVersion > 0 {
		if err := verifyBaselineSchema(ctx, conn, baselineVersion); err != nil {
			return err
		}
		if err := baseline(ctx, conn, migrations, baselineVersion); err != nil {
			return err
		}
	}

	for _, migration := range migrations {
		var checksum []byte
		err := conn.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version = $1`, migration.Version).Scan(&checksum)
		if err == nil {
			if string(checksum) != string(migration.Checksum[:]) {
				return fmt.Errorf("migration %04d checksum mismatch; do not edit an applied migration", migration.Version)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read migration %04d ledger: %w", migration.Version, err)
		}
		if err := applyOne(ctx, conn, migration); err != nil {
			return err
		}
	}
	return nil
}

func verifyBaselineSchema(ctx context.Context, conn *pgx.Conn, version int) error {
	required := []string{"users", "user_sessions", "upload_sessions", "assets", "upload_events", "signed_manifests"}
	if version >= 2 {
		required = append(required, "upload_sessions.asset_id", "user_sessions.session_family_id", "user_sessions.parent_session_id", "user_sessions.reused_at")
	}
	if version >= 3 {
		required = append(required, "upload_sessions.verification_worker_id", "upload_sessions.verification_claimed_at", "upload_sessions.verification_lease_until")
	}
	if version >= 4 {
		required = append(required, "upload_sessions.verification_claim_token")
	}
	for _, item := range required {
		parts := strings.Split(item, ".")
		if len(parts) == 1 {
			var table *string
			if err := conn.QueryRow(ctx, `SELECT to_regclass('public.' || $1)::text`, parts[0]).Scan(&table); err != nil {
				return fmt.Errorf("inspect baseline table %s: %w", item, err)
			}
			if table == nil {
				return fmt.Errorf("refusing baseline %d: required table %s is absent", version, item)
			}
			continue
		}
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`, parts[0], parts[1]).Scan(&exists); err != nil {
			return fmt.Errorf("inspect baseline column %s: %w", item, err)
		}
		if !exists {
			return fmt.Errorf("refusing baseline %d: required column %s is absent", version, item)
		}
	}
	return nil
}

func baseline(ctx context.Context, conn *pgx.Conn, migrations []Migration, baselineVersion int) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	found := false
	for _, migration := range migrations {
		if migration.Version > baselineVersion {
			break
		}
		found = true
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`, migration.Version, migration.Name, migration.Checksum[:]); err != nil {
			return fmt.Errorf("baseline migration %04d: %w", migration.Version, err)
		}
	}
	if !found {
		return fmt.Errorf("baseline version %d is not a known migration", baselineVersion)
	}
	return tx.Commit(ctx)
}

func applyOne(ctx context.Context, conn *pgx.Conn, migration Migration) error {
	body, err := transactionalBody(migration.SQL)
	if err != nil {
		return fmt.Errorf("migration %04d %s: %w", migration.Version, migration.Name, err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %04d: %w", migration.Version, err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, body); err != nil {
		return fmt.Errorf("apply migration %04d %s: %w", migration.Version, migration.Name, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`, migration.Version, migration.Name, migration.Checksum[:]); err != nil {
		return fmt.Errorf("record migration %04d: %w", migration.Version, err)
	}
	return tx.Commit(ctx)
}

func transactionalBody(sql string) (string, error) {
	trimmed := strings.TrimSpace(sql)
	if !strings.HasPrefix(trimmed, "BEGIN;") || !strings.HasSuffix(trimmed, "COMMIT;") {
		return "", errors.New("migration must begin with BEGIN; and end with COMMIT;")
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(trimmed, "BEGIN;")), "COMMIT;"))
	if body == "" {
		return "", errors.New("migration body is empty")
	}
	return body, nil
}
