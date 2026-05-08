// Package sqlite provides a SQLite-backed implementation of store.Store
// using modernc.org/sqlite (pure Go, no CGO required).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rbaliyan/postbox/internal/store"
	_ "modernc.org/sqlite"
)

var _ store.Store = (*Store)(nil)

type Store struct {
	dsn string
	db  *sql.DB
}

func New(dsn string) *Store {
	return &Store{dsn: dsn}
}

func (s *Store) Connect(ctx context.Context) error {
	db, err := sql.Open("sqlite", s.dsn)
	if err != nil {
		return fmt.Errorf("sqlite: open %q: %w", s.dsn, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return fmt.Errorf("sqlite: ping: %w", err)
	}
	s.db = db
	return s.migrate(ctx)
}

func (s *Store) Close(_ context.Context) error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS nodes (
    id          TEXT PRIMARY KEY,
    private_key TEXT NOT NULL DEFAULT '',
    public_key  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS domains (
    name       TEXT PRIMARY KEY,
    node_id    TEXT    NOT NULL DEFAULT '',
    is_default INTEGER NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_domains_default ON domains (is_default) WHERE is_default = 1;

CREATE TABLE IF NOT EXISTS users (
    email      TEXT PRIMARY KEY,
    type       TEXT NOT NULL DEFAULT '',
    public_key TEXT NOT NULL DEFAULT '',
    metadata   TEXT NOT NULL DEFAULT '{}',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS webhook_delivery_jobs (
    message_id    TEXT      NOT NULL,
    recipient_id  TEXT      NOT NULL,
    endpoint_url  TEXT      NOT NULL DEFAULT '',
    status        TEXT      NOT NULL DEFAULT 'pending',
    attempts      INTEGER   NOT NULL DEFAULT 0,
    max_attempts  INTEGER   NOT NULL DEFAULT 5,
    last_error    TEXT      NOT NULL DEFAULT '',
    next_retry_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    delivered_at  TIMESTAMP,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (message_id, recipient_id)
);

CREATE INDEX IF NOT EXISTS idx_wdj_pending
    ON webhook_delivery_jobs (next_retry_at)
    WHERE status IN ('pending', 'failed');
`

// migrations runs idempotent ALTER TABLE statements for databases created by
// older schema versions. "duplicate column name" errors are ignored.
var migrations = []string{
	// Phase 2: add key columns to nodes.
	`ALTER TABLE nodes ADD COLUMN private_key TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE nodes ADD COLUMN public_key  TEXT NOT NULL DEFAULT ''`,
	// Phase 3: promote public_key and type to users; drop legacy agents table.
	`ALTER TABLE users ADD COLUMN public_key TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE users ADD COLUMN type TEXT NOT NULL DEFAULT ''`,
	`DROP TABLE IF EXISTS agents`,
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	for _, m := range migrations {
		if _, err := s.db.ExecContext(ctx, m); err != nil {
			if !strings.Contains(err.Error(), "duplicate column name") {
				return err
			}
		}
	}
	return nil
}

// ---- Node ----

func (s *Store) SaveNode(ctx context.Context, node store.Node) error {
	if node.ID == "" {
		return fmt.Errorf("sqlite: node id is required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO nodes (id, private_key, public_key) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		    private_key = CASE WHEN excluded.private_key != '' THEN excluded.private_key ELSE nodes.private_key END,
		    public_key  = CASE WHEN excluded.public_key  != '' THEN excluded.public_key  ELSE nodes.public_key  END`,
		node.ID, node.PrivateKeyB64, node.PublicKeyB64)
	return err
}

func (s *Store) GetNode(ctx context.Context, id string) (store.Node, error) {
	var n store.Node
	err := s.db.QueryRowContext(ctx,
		`SELECT id, private_key, public_key FROM nodes WHERE id = ?`, id).
		Scan(&n.ID, &n.PrivateKeyB64, &n.PublicKeyB64)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Node{}, store.ErrNotFound
	}
	if err != nil {
		return store.Node{}, fmt.Errorf("sqlite: get node: %w", err)
	}
	return n, nil
}

// ---- Domain ----

func (s *Store) SaveDomain(ctx context.Context, domain store.Domain) error {
	isDefault := 0
	if domain.IsDefault {
		isDefault = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO domains (name, node_id, is_default) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET node_id = excluded.node_id, is_default = excluded.is_default`,
		domain.Name, domain.NodeID, isDefault)
	return err
}

func (s *Store) GetDomain(ctx context.Context, name string) (store.Domain, error) {
	var d store.Domain
	var isDefault int
	err := s.db.QueryRowContext(ctx,
		`SELECT name, node_id, is_default FROM domains WHERE name = ?`, name).
		Scan(&d.Name, &d.NodeID, &isDefault)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Domain{}, store.ErrNotFound
	}
	if err != nil {
		return store.Domain{}, err
	}
	d.IsDefault = isDefault != 0
	return d, nil
}

func (s *Store) GetDefaultDomain(ctx context.Context) (store.Domain, error) {
	var d store.Domain
	var isDefault int
	err := s.db.QueryRowContext(ctx,
		`SELECT name, node_id, is_default FROM domains WHERE is_default = 1 LIMIT 1`).
		Scan(&d.Name, &d.NodeID, &isDefault)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Domain{}, store.ErrNotFound
	}
	if err != nil {
		return store.Domain{}, err
	}
	d.IsDefault = isDefault != 0
	return d, nil
}

func (s *Store) ListDomains(ctx context.Context) ([]store.Domain, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, node_id, is_default FROM domains ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var domains []store.Domain
	for rows.Next() {
		var d store.Domain
		var isDefault int
		if err := rows.Scan(&d.Name, &d.NodeID, &isDefault); err != nil {
			return nil, err
		}
		d.IsDefault = isDefault != 0
		domains = append(domains, d)
	}
	return domains, rows.Err()
}

func (s *Store) CountDomains(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM domains`).Scan(&count)
	return count, err
}

// ---- User ----

func (s *Store) SaveUser(ctx context.Context, user store.User) error {
	meta, err := json.Marshal(user.Metadata)
	if err != nil {
		return fmt.Errorf("sqlite: marshal metadata: %w", err)
	}
	createdAt := user.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO users (email, type, public_key, metadata, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(email) DO UPDATE SET
		    type       = excluded.type,
		    public_key = excluded.public_key,
		    metadata   = excluded.metadata`,
		user.Email, user.Type, user.PublicKeyB64, string(meta), createdAt)
	return err
}

func (s *Store) GetUser(ctx context.Context, email string) (store.User, error) {
	var u store.User
	var metaJSON string
	err := s.db.QueryRowContext(ctx,
		`SELECT email, type, public_key, metadata, created_at FROM users WHERE email = ?`, email).
		Scan(&u.Email, &u.Type, &u.PublicKeyB64, &metaJSON, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.User{}, store.ErrNotFound
	}
	if err != nil {
		return store.User{}, err
	}
	if err := json.Unmarshal([]byte(metaJSON), &u.Metadata); err != nil {
		return store.User{}, fmt.Errorf("sqlite: unmarshal metadata: %w", err)
	}
	return u, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]store.User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT email, type, public_key, metadata, created_at FROM users ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanUsers(rows)
}

func (s *Store) SearchUsers(ctx context.Context, filters map[string]string) ([]store.User, error) {
	if len(filters) == 0 {
		return s.ListUsers(ctx)
	}

	// Sort keys so the generated query is deterministic across calls.
	keys := make([]string, 0, len(filters))
	for k := range filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var clauses []string
	var args []any
	for _, k := range keys {
		v := filters[k]
		if k == "type" {
			clauses = append(clauses, "type LIKE ?")
		} else if store.IsValidMetaKey(k) {
			clauses = append(clauses, "json_extract(metadata, '$."+k+"') LIKE ?")
		} else {
			continue
		}
		args = append(args, "%"+v+"%")
	}

	if len(clauses) == 0 {
		return s.ListUsers(ctx)
	}

	query := `SELECT email, type, public_key, metadata, created_at FROM users WHERE ` +
		strings.Join(clauses, " AND ") + ` ORDER BY email`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanUsers(rows)
}

func (s *Store) CountUsers(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count)
	return count, err
}

func (s *Store) scanUsers(rows *sql.Rows) ([]store.User, error) {
	var users []store.User
	for rows.Next() {
		var u store.User
		var metaJSON string
		if err := rows.Scan(&u.Email, &u.Type, &u.PublicKeyB64, &metaJSON, &u.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(metaJSON), &u.Metadata); err != nil {
			return nil, fmt.Errorf("sqlite: unmarshal metadata: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// ---- Webhook delivery jobs ----

func (s *Store) SaveDeliveryJob(ctx context.Context, job store.DeliveryJob) error {
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	if job.UpdatedAt.IsZero() {
		job.UpdatedAt = now
	}
	if job.MaxAttempts == 0 {
		job.MaxAttempts = 5
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO webhook_delivery_jobs
		    (message_id, recipient_id, endpoint_url, status, attempts, max_attempts,
		     last_error, next_retry_at, delivered_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(message_id, recipient_id) DO NOTHING`,
		job.MessageID, job.RecipientID, job.EndpointURL,
		string(job.Status), job.Attempts, job.MaxAttempts,
		job.LastError, job.NextRetryAt, job.DeliveredAt,
		job.CreatedAt, job.UpdatedAt)
	return err
}

func (s *Store) UpdateDeliveryJob(ctx context.Context, job store.DeliveryJob) error {
	job.UpdatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE webhook_delivery_jobs
		SET status = ?, attempts = ?, last_error = ?,
		    next_retry_at = ?, delivered_at = ?, updated_at = ?
		WHERE message_id = ? AND recipient_id = ?`,
		string(job.Status), job.Attempts, job.LastError,
		job.NextRetryAt, job.DeliveredAt, job.UpdatedAt,
		job.MessageID, job.RecipientID)
	return err
}

func (s *Store) GetDeliveryJob(ctx context.Context, messageID, recipientID string) (store.DeliveryJob, error) {
	var j store.DeliveryJob
	var status string
	err := s.db.QueryRowContext(ctx, `
		SELECT message_id, recipient_id, endpoint_url, status, attempts, max_attempts,
		       last_error, next_retry_at, delivered_at, created_at, updated_at
		FROM webhook_delivery_jobs
		WHERE message_id = ? AND recipient_id = ?`, messageID, recipientID).
		Scan(&j.MessageID, &j.RecipientID, &j.EndpointURL,
			&status, &j.Attempts, &j.MaxAttempts,
			&j.LastError, &j.NextRetryAt, &j.DeliveredAt,
			&j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.DeliveryJob{}, store.ErrNotFound
	}
	if err != nil {
		return store.DeliveryJob{}, fmt.Errorf("sqlite: get delivery job: %w", err)
	}
	j.Status = store.DeliveryStatus(status)
	return j, nil
}

func (s *Store) ListPendingDeliveryJobs(ctx context.Context, before time.Time, limit int) ([]store.DeliveryJob, error) {
	q := `
		SELECT message_id, recipient_id, endpoint_url, status, attempts, max_attempts,
		       last_error, next_retry_at, delivered_at, created_at, updated_at
		FROM webhook_delivery_jobs
		WHERE status IN ('pending', 'failed') AND next_retry_at <= ?
		ORDER BY next_retry_at`
	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = s.db.QueryContext(ctx, q+" LIMIT ?", before, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, q, before)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanJobs(rows)
}

func (s *Store) ListDeliveryJobsByRecipient(ctx context.Context, recipientID string, status store.DeliveryStatus) ([]store.DeliveryJob, error) {
	var rows *sql.Rows
	var err error
	const base = `
		SELECT message_id, recipient_id, endpoint_url, status, attempts, max_attempts,
		       last_error, next_retry_at, delivered_at, created_at, updated_at
		FROM webhook_delivery_jobs WHERE recipient_id = ?`
	if status == "" {
		rows, err = s.db.QueryContext(ctx, base+" ORDER BY created_at DESC", recipientID)
	} else {
		rows, err = s.db.QueryContext(ctx, base+" AND status = ? ORDER BY created_at DESC", recipientID, string(status))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanJobs(rows)
}

func (s *Store) scanJobs(rows *sql.Rows) ([]store.DeliveryJob, error) {
	var jobs []store.DeliveryJob
	for rows.Next() {
		var j store.DeliveryJob
		var status string
		if err := rows.Scan(&j.MessageID, &j.RecipientID, &j.EndpointURL,
			&status, &j.Attempts, &j.MaxAttempts,
			&j.LastError, &j.NextRetryAt, &j.DeliveredAt,
			&j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		j.Status = store.DeliveryStatus(status)
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
