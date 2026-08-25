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
	if version < 1 || version > 9 {
		return fmt.Errorf("refusing baseline %d: schema fingerprint is defined only for versions 1 through 9", version)
	}

	columns := []baselineColumn{
		{"users", "id", "uuid", false},
		{"users", "email", "text", false},
		{"users", "password_hash", "text", false},
		{"users", "quota_bytes", "bigint", true},
		{"user_sessions", "id", "uuid", false},
		{"user_sessions", "user_id", "uuid", false},
		{"user_sessions", "refresh_token_sha256", "bytea", false},
		{"upload_sessions", "id", "uuid", false},
		{"upload_sessions", "owner_id", "uuid", false},
		{"upload_sessions", "expected_size", "bigint", false},
		{"upload_sessions", "received_size", "bigint", false},
		{"upload_sessions", "client_sha256", "bytea", false},
		{"upload_sessions", "server_sha256", "bytea", true},
		{"upload_sessions", "state", "text", false},
		{"assets", "id", "uuid", false},
		{"assets", "owner_id", "uuid", false},
		{"assets", "storage_key", "text", false},
		{"assets", "byte_size", "bigint", false},
		{"assets", "content_sha256", "bytea", false},
		{"upload_events", "sequence_id", "bigint", false},
		{"upload_events", "upload_session_id", "uuid", false},
		{"asset_integrity_checks", "asset_id", "uuid", false},
		{"signed_manifests", "payload_sha256", "bytea", false},
	}
	if version >= 2 {
		columns = append(columns,
			baselineColumn{"upload_sessions", "asset_id", "uuid", true},
			baselineColumn{"user_sessions", "session_family_id", "uuid", false},
			baselineColumn{"user_sessions", "parent_session_id", "uuid", true},
			baselineColumn{"user_sessions", "reused_at", "timestamp with time zone", true},
		)
	}
	if version >= 3 {
		columns = append(columns,
			baselineColumn{"upload_sessions", "verification_worker_id", "text", true},
			baselineColumn{"upload_sessions", "verification_claimed_at", "timestamp with time zone", true},
			baselineColumn{"upload_sessions", "verification_lease_until", "timestamp with time zone", true},
		)
	}
	if version >= 4 {
		columns = append(columns, baselineColumn{"upload_sessions", "verification_claim_token", "uuid", true})
	}
	if version >= 6 {
		columns = append(columns,
			baselineColumn{"login_throttles", "identity_hash", "bytea", false},
			baselineColumn{"login_throttles", "attempt_count", "integer", false},
			baselineColumn{"upload_session_throttles", "owner_id", "uuid", false},
			baselineColumn{"upload_session_throttles", "create_count", "integer", false},
		)
	}
	if version >= 7 {
		columns = append(columns,
			baselineColumn{"user_mfa_totp", "user_id", "uuid", false},
			baselineColumn{"user_mfa_totp", "encrypted_secret", "bytea", false},
			baselineColumn{"user_mfa_totp", "nonce", "bytea", false},
			baselineColumn{"user_mfa_totp", "confirmed_at", "timestamp with time zone", true},
			baselineColumn{"user_mfa_totp", "last_used_counter", "bigint", true},
			baselineColumn{"user_mfa_recovery_codes", "user_id", "uuid", false},
			baselineColumn{"user_mfa_recovery_codes", "code_hash", "bytea", false},
			baselineColumn{"user_mfa_recovery_codes", "used_at", "timestamp with time zone", true},
			baselineColumn{"mfa_challenges", "challenge_hash", "bytea", false},
			baselineColumn{"mfa_challenges", "user_id", "uuid", false},
			baselineColumn{"mfa_challenges", "expires_at", "timestamp with time zone", false},
			baselineColumn{"mfa_challenges", "attempts_remaining", "integer", false},
			baselineColumn{"mfa_challenges", "consumed_at", "timestamp with time zone", true},
		)
	}
	if version >= 8 {
		columns = append(columns,
			baselineColumn{"mfa_action_throttles", "user_id", "uuid", false},
			baselineColumn{"mfa_action_throttles", "action", "text", false},
			baselineColumn{"mfa_action_throttles", "window_started_at", "timestamp with time zone", false},
			baselineColumn{"mfa_action_throttles", "attempt_count", "integer", false},
			baselineColumn{"mfa_action_throttles", "blocked_until", "timestamp with time zone", true},
			baselineColumn{"mfa_action_throttles", "updated_at", "timestamp with time zone", false},
		)
	}
	if version >= 9 {
		columns = append(columns,
			baselineColumn{"user_sessions", "refresh_retry_request_sha256", "bytea", true},
			baselineColumn{"user_sessions", "refresh_retry_ciphertext", "bytea", true},
			baselineColumn{"user_sessions", "refresh_retry_nonce", "bytea", true},
			baselineColumn{"user_sessions", "refresh_retry_until", "timestamp with time zone", true},
		)
	}
	for _, expected := range columns {
		if err := verifyBaselineColumn(ctx, conn, expected); err != nil {
			return fmt.Errorf("refusing baseline %d: %w", version, err)
		}
	}

	indexes := []baselineIndex{
		{"users_email_unique_ci", "users", []string{"lower(email)"}},
		{"user_sessions_active_by_user", "user_sessions", []string{"user_id", "expires_at", "revoked_at is null"}},
		{"upload_sessions_expiration", "upload_sessions", []string{"expires_at", "state"}},
		{"assets_timeline", "assets", []string{"owner_id", "captured_at", "deleted_at is null"}},
		{"upload_events_by_session", "upload_events", []string{"upload_session_id", "sequence_id"}},
		{"asset_integrity_checks_latest", "asset_integrity_checks", []string{"asset_id", "checked_at"}},
	}
	if version >= 1 && version < 5 {
		indexes = append(indexes, baselineIndex{"upload_sessions_reconciliation", "upload_sessions", []string{"state", "updated_at", "committing"}})
	}
	if version >= 2 {
		indexes = append(indexes,
			baselineIndex{"upload_sessions_asset_id", "upload_sessions", []string{"asset_id", "is not null"}},
			baselineIndex{"user_sessions_family_active", "user_sessions", []string{"session_family_id", "expires_at", "revoked_at is null"}},
		)
	}
	if version >= 3 && version < 5 {
		indexes = append(indexes,
			baselineIndex{"upload_sessions_verification_claim", "upload_sessions", []string{"updated_at", "verification_lease_until is null"}},
			baselineIndex{"upload_sessions_verification_lease_expiry", "upload_sessions", []string{"verification_lease_until", "updated_at"}},
		)
	}
	if version >= 4 {
		indexes = append(indexes, baselineIndex{"upload_sessions_verification_claim_token", "upload_sessions", []string{"verification_claim_token", "is not null"}})
	}
	if version >= 5 {
		indexes = append(indexes,
			baselineIndex{"upload_sessions_reconciliation", "upload_sessions", []string{"state", "updated_at", "quarantining"}},
			baselineIndex{"upload_sessions_verification_claim", "upload_sessions", []string{"updated_at", "verification_lease_until is null", "quarantining"}},
			baselineIndex{"upload_sessions_verification_lease_expiry", "upload_sessions", []string{"verification_lease_until", "updated_at", "quarantining"}},
		)
	}
	if version >= 6 {
		indexes = append(indexes,
			baselineIndex{"login_throttles_cleanup", "login_throttles", []string{"updated_at"}},
			baselineIndex{"upload_session_throttles_cleanup", "upload_session_throttles", []string{"updated_at"}},
			baselineIndex{"upload_sessions_owner_active", "upload_sessions", []string{"owner_id", "state", "quarantining"}},
		)
	}
	if version >= 7 {
		indexes = append(indexes,
			baselineIndex{"user_mfa_recovery_codes_unused", "user_mfa_recovery_codes", []string{"user_id", "id", "used_at is null"}},
			baselineIndex{"mfa_challenges_active", "mfa_challenges", []string{"challenge_hash", "expires_at", "consumed_at is null", "attempts_remaining > 0"}},
			baselineIndex{"mfa_challenges_user_recent", "mfa_challenges", []string{"user_id", "created_at"}},
			baselineIndex{"mfa_challenges_cleanup", "mfa_challenges", []string{"created_at"}},
		)
	}
	if version >= 8 {
		indexes = append(indexes,
			baselineIndex{"mfa_action_throttles_cleanup", "mfa_action_throttles", []string{"updated_at"}},
		)
	}
	if version >= 9 {
		indexes = append(indexes,
			baselineIndex{"user_sessions_refresh_retry_cleanup", "user_sessions", []string{"refresh_retry_until", "is not null"}},
		)
	}
	for _, expected := range indexes {
		if err := verifyBaselineIndex(ctx, conn, expected); err != nil {
			return fmt.Errorf("refusing baseline %d: %w", version, err)
		}
	}

	constraints := []baselineConstraint{
		{"users_pkey", "users", "p", []string{"primary key (id)"}},
		{"user_sessions_pkey", "user_sessions", "p", []string{"primary key (id)"}},
		{"upload_sessions_pkey", "upload_sessions", "p", []string{"primary key (id)"}},
		{"assets_pkey", "assets", "p", []string{"primary key (id)"}},
		{"upload_events_pkey", "upload_events", "p", []string{"primary key (sequence_id)"}},
		{"asset_integrity_checks_pkey", "asset_integrity_checks", "p", []string{"primary key (id)"}},
		{"signed_manifests_pkey", "signed_manifests", "p", []string{"primary key (id)"}},
		{"user_sessions_refresh_token_sha256_key", "user_sessions", "u", []string{"unique (refresh_token_sha256)"}},
		{"upload_sessions_owner_id_client_asset_id_key", "upload_sessions", "u", []string{"unique (owner_id, client_asset_id)"}},
		{"upload_sessions_transport_transport_resource_id_key", "upload_sessions", "u", []string{"unique (transport, transport_resource_id)"}},
		{"assets_upload_session_id_key", "assets", "u", []string{"unique (upload_session_id)"}},
		{"assets_owner_id_storage_key_key", "assets", "u", []string{"unique (owner_id, storage_key)"}},
		{"assets_owner_id_content_sha256_key", "assets", "u", []string{"unique (owner_id, content_sha256)"}},
		{"signed_manifests_object_key_key", "signed_manifests", "u", []string{"unique (object_key)"}},
		{"user_sessions_user_id_fkey", "user_sessions", "f", []string{"foreign key (user_id)", "references users(id)"}},
		{"upload_sessions_owner_id_fkey", "upload_sessions", "f", []string{"foreign key (owner_id)", "references users(id)"}},
		{"assets_owner_id_fkey", "assets", "f", []string{"foreign key (owner_id)", "references users(id)"}},
		{"assets_upload_session_id_fkey", "assets", "f", []string{"foreign key (upload_session_id)", "references upload_sessions(id)"}},
		{"upload_events_upload_session_id_fkey", "upload_events", "f", []string{"foreign key (upload_session_id)", "references upload_sessions(id)"}},
		{"upload_events_owner_id_fkey", "upload_events", "f", []string{"foreign key (owner_id)", "references users(id)"}},
		{"asset_integrity_checks_asset_id_fkey", "asset_integrity_checks", "f", []string{"foreign key (asset_id)", "references assets(id)"}},
	}
	if version >= 2 {
		constraints = append(constraints,
			baselineConstraint{"upload_sessions_asset_id_fkey", "upload_sessions", "f", []string{"foreign key (asset_id)", "references assets(id)"}},
			baselineConstraint{"user_sessions_parent_session_id_fkey", "user_sessions", "f", []string{"foreign key (parent_session_id)", "references user_sessions(id)"}},
		)
	}
	if version >= 6 {
		constraints = append(constraints,
			baselineConstraint{"login_throttles_pkey", "login_throttles", "p", []string{"primary key (identity_hash)"}},
			baselineConstraint{"upload_session_throttles_pkey", "upload_session_throttles", "p", []string{"primary key (owner_id)"}},
			baselineConstraint{"upload_session_throttles_owner_id_fkey", "upload_session_throttles", "f", []string{"foreign key (owner_id)", "references users(id)", "on delete cascade"}},
		)
	}
	if version >= 7 {
		constraints = append(constraints,
			baselineConstraint{"user_mfa_totp_pkey", "user_mfa_totp", "p", []string{"primary key (user_id)"}},
			baselineConstraint{"user_mfa_recovery_codes_pkey", "user_mfa_recovery_codes", "p", []string{"primary key (id)"}},
			baselineConstraint{"mfa_challenges_pkey", "mfa_challenges", "p", []string{"primary key (id)"}},
			baselineConstraint{"user_mfa_recovery_codes_user_id_code_hash_key", "user_mfa_recovery_codes", "u", []string{"unique (user_id, code_hash)"}},
			baselineConstraint{"mfa_challenges_challenge_hash_key", "mfa_challenges", "u", []string{"unique (challenge_hash)"}},
			baselineConstraint{"user_mfa_totp_user_id_fkey", "user_mfa_totp", "f", []string{"foreign key (user_id)", "references users(id)", "on delete cascade"}},
			baselineConstraint{"user_mfa_recovery_codes_user_id_fkey", "user_mfa_recovery_codes", "f", []string{"foreign key (user_id)", "references users(id)", "on delete cascade"}},
			baselineConstraint{"mfa_challenges_user_id_fkey", "mfa_challenges", "f", []string{"foreign key (user_id)", "references users(id)", "on delete cascade"}},
		)
	}
	if version >= 8 {
		constraints = append(constraints,
			baselineConstraint{"mfa_action_throttles_pkey", "mfa_action_throttles", "p", []string{"primary key (user_id, action)"}},
			baselineConstraint{"mfa_action_throttles_user_id_fkey", "mfa_action_throttles", "f", []string{"foreign key (user_id)", "references users(id)", "on delete cascade"}},
		)
	}
	if version >= 9 {
		constraints = append(constraints,
			baselineConstraint{"user_sessions_refresh_retry_shape", "user_sessions", "c", []string{"refresh_retry_request_sha256", "refresh_retry_ciphertext", "refresh_retry_nonce", "refresh_retry_until", "octet_length(refresh_retry_request_sha256)", "32", "octet_length(refresh_retry_nonce)", "12"}},
		)
	}
	for _, expected := range constraints {
		if err := verifyBaselineConstraint(ctx, conn, expected); err != nil {
			return fmt.Errorf("refusing baseline %d: %w", version, err)
		}
	}

	checks := []baselineCheck{
		{"users", "role", []string{"admin", "member"}},
		{"users", "state", []string{"invited", "active", "disabled", "deleting"}},
		{"upload_sessions", "expected_size", []string{"expected_size", ">= 0"}},
		{"upload_sessions", "client_sha256", []string{"octet_length(client_sha256)", "32"}},
		{"upload_sessions", "available storage", []string{"state", "available", "final_storage_key", "is not null"}},
		{"assets", "content_sha256", []string{"octet_length(content_sha256)", "32"}},
		{"signed_manifests", "signature", []string{"octet_length(signature)", "64"}},
		{"upload_events", "event types", []string{"created", "upload_started", "available", "quarantined", "reconciled"}},
	}
	if version >= 2 {
		checks = append(checks, baselineCheck{"upload_sessions", "available asset", []string{"state", "available", "asset_id", "is not null"}})
	}
	if version >= 5 {
		checks = append(checks,
			baselineCheck{"upload_sessions", "quarantining state", []string{"quarantining"}},
			baselineCheck{"upload_events", "quarantine intent event", []string{"quarantine_started"}},
		)
	}
	if version >= 6 {
		checks = append(checks,
			baselineCheck{"login_throttles", "identity hash", []string{"octet_length(identity_hash)", "32"}},
			baselineCheck{"upload_session_throttles", "create count", []string{"create_count", ">= 0"}},
		)
	}
	if version >= 7 {
		checks = append(checks,
			baselineCheck{"user_mfa_totp", "nonce length", []string{"octet_length(nonce)", "12"}},
			baselineCheck{"user_mfa_recovery_codes", "recovery hash", []string{"octet_length(code_hash)", "32"}},
			baselineCheck{"mfa_challenges", "challenge hash", []string{"octet_length(challenge_hash)", "32"}},
			baselineCheck{"mfa_challenges", "attempts nonnegative", []string{"attempts_remaining", ">= 0"}},
		)
	}
	if version >= 8 {
		actions := []string{"confirm", "recovery", "disable"}
		if version >= 9 {
			actions = append(actions, "enroll")
		}
		checks = append(checks,
			baselineCheck{"mfa_action_throttles", "action domain", actions},
			baselineCheck{"mfa_action_throttles", "attempts nonnegative", []string{"attempt_count", ">= 0"}},
		)
	}
	for _, expected := range checks {
		if err := verifyBaselineCheck(ctx, conn, expected); err != nil {
			return fmt.Errorf("refusing baseline %d: %w", version, err)
		}
	}
	return nil
}

type baselineColumn struct {
	table    string
	column   string
	dataType string
	nullable bool
}

type baselineIndex struct {
	name      string
	table     string
	fragments []string
}

type baselineCheck struct {
	table     string
	label     string
	fragments []string
}

type baselineConstraint struct {
	name           string
	table          string
	constraintType string
	fragments      []string
}

func verifyBaselineColumn(ctx context.Context, conn *pgx.Conn, expected baselineColumn) error {
	var dataType, nullable string
	err := conn.QueryRow(ctx, `
        SELECT data_type, is_nullable
        FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`, expected.table, expected.column).Scan(&dataType, &nullable)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("required column %s.%s is absent", expected.table, expected.column)
	}
	if err != nil {
		return fmt.Errorf("inspect column %s.%s: %w", expected.table, expected.column, err)
	}
	if dataType != expected.dataType || (nullable == "YES") != expected.nullable {
		return fmt.Errorf("column %s.%s fingerprint mismatch: got type=%q nullable=%s", expected.table, expected.column, dataType, nullable)
	}
	return nil
}

func verifyBaselineIndex(ctx context.Context, conn *pgx.Conn, expected baselineIndex) error {
	var definition string
	err := conn.QueryRow(ctx, `
        SELECT pg_get_indexdef(indexrelid)
        FROM pg_index
        WHERE indexrelid = to_regclass('public.' || $1)
          AND indrelid = to_regclass('public.' || $2)`, expected.name, expected.table).Scan(&definition)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("required index %s is absent or belongs to a different table", expected.name)
	}
	if err != nil {
		return fmt.Errorf("inspect index %s: %w", expected.name, err)
	}
	normalized := normalizeCatalogDefinition(definition)
	for _, fragment := range expected.fragments {
		if !strings.Contains(normalized, normalizeCatalogDefinition(fragment)) {
			return fmt.Errorf("index %s fingerprint mismatch: missing %q", expected.name, fragment)
		}
	}
	return nil
}

func verifyBaselineConstraint(ctx context.Context, conn *pgx.Conn, expected baselineConstraint) error {
	var definition string
	err := conn.QueryRow(ctx, `
        SELECT pg_get_constraintdef(oid)
        FROM pg_constraint
        WHERE conrelid = to_regclass('public.' || $1)
          AND conname = $2 AND contype::text = $3`,
		expected.table, expected.name, expected.constraintType,
	).Scan(&definition)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("required constraint %s on %s is absent", expected.name, expected.table)
	}
	if err != nil {
		return fmt.Errorf("inspect constraint %s on %s: %w", expected.name, expected.table, err)
	}
	normalized := normalizeCatalogDefinition(definition)
	for _, fragment := range expected.fragments {
		if !strings.Contains(normalized, normalizeCatalogDefinition(fragment)) {
			return fmt.Errorf("constraint %s fingerprint mismatch: got %q", expected.name, definition)
		}
	}
	return nil
}

func verifyBaselineCheck(ctx context.Context, conn *pgx.Conn, expected baselineCheck) error {
	rows, err := conn.Query(ctx, `
        SELECT pg_get_constraintdef(oid)
        FROM pg_constraint
        WHERE conrelid = to_regclass('public.' || $1) AND contype = 'c'`, expected.table)
	if err != nil {
		return fmt.Errorf("inspect checks on %s: %w", expected.table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var definition string
		if err := rows.Scan(&definition); err != nil {
			return err
		}
		normalized := normalizeCatalogDefinition(definition)
		matched := true
		for _, fragment := range expected.fragments {
			if !strings.Contains(normalized, normalizeCatalogDefinition(fragment)) {
				matched = false
				break
			}
		}
		if matched {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return fmt.Errorf("required check %s on %s does not match expected definition", expected.label, expected.table)
}

func normalizeCatalogDefinition(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
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
