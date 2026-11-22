package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrStackNameTaken indicates that the stack ID or hostname is already claimed.
var ErrStackNameTaken = errors.New("stack name already claimed")

// Status contains the public lifecycle state and recovery metadata.
type Status struct {
	Stack
	TemplateName string          `json:"template_name,omitempty"`
	ImagesReady  bool            `json:"images_prepared,omitempty"`
	Retryable    bool            `json:"retryable,omitempty"`
	HasManifest  bool            `json:"-"`
	HasImages    bool            `json:"-"`
	APIOwned     bool            `json:"-"`
	Pending      bool            `json:"publication_pending,omitempty"`
	FailurePhase string          `json:"failure_phase,omitempty"`
	Failure      string          `json:"failure,omitempty"`
	URL          string          `json:"url,omitempty"`
	Bindings     json.RawMessage `json:"bindings,omitempty"`
	ReadyAt      string          `json:"ready_at,omitempty"`
	ExpiresAt    string          `json:"expires_at,omitempty"`
}

// CreateAPI claims the ID and hostname before work starts.
func (l *Ledger) CreateAPI(ctx context.Context, id, host string, duration time.Duration) error {
	return l.CreateAPIWithEnvironment(ctx, id, host, duration, nil)
}

// CreateAPIWithEnvironment saves launch settings atomically with the record.
func (l *Ledger) CreateAPIWithEnvironment(ctx context.Context, id, host string, duration time.Duration, env Environment) error {
	return l.CreateLaunch(ctx, id, host, duration, env, "", nil)
}

// CreateLaunch records a fresh API launch before acquiring its manifest.
func (l *Ledger) CreateLaunch(ctx context.Context, id, host string, duration time.Duration, env Environment, template string, request *LaunchRequest) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	input, err := encodeLaunchRequest(request)
	if err != nil {
		return err
	}
	return l.createNamedStack(ctx, id, `INSERT INTO stacks (id, hostname, duration_ns, state, api_owned, environment, template_name, launch_request)
		VALUES (?, ?, ?, 'launching', 1, ?, ?, ?) ON CONFLICT DO NOTHING`, id, host, int64(duration), data, template, input)
}

func (l *Ledger) createNamedStack(ctx context.Context, id, statement string, args ...any) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Keep the name reserved after removal so an old URL cannot reach a new stack.
	if err := requireInsert(tx.ExecContext(ctx, `INSERT INTO stack_launch_names (id) VALUES (?) ON CONFLICT DO NOTHING`, id)); err != nil {
		return err
	}
	if err := requireInsert(tx.ExecContext(ctx, statement, args...)); err != nil {
		return err
	}
	return tx.Commit()
}

func requireInsert(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return ErrStackNameTaken
	}
	return err
}

const statusColumns = `id, hostname, duration_ns, state, api_owned, publication_pending, failure_phase, failure, url, bindings, ready_at, expires_at, template_name,
	manifest IS NOT NULL, images IS NOT NULL, (api_owned = 1 AND state = 'failed' AND ` + retryInputs + `)`

type statusScanner interface{ Scan(...any) error }

func scanStatus(row statusScanner) (Status, error) {
	var s Status
	var duration int64
	var bindings []byte
	err := row.Scan(&s.ID, &s.Hostname, &duration, &s.State, &s.APIOwned, &s.Pending, &s.FailurePhase, &s.Failure, &s.URL, &bindings, &s.ReadyAt, &s.ExpiresAt, &s.TemplateName, &s.HasManifest, &s.HasImages, &s.Retryable)
	s.Duration = time.Duration(duration)
	s.Bindings = bindings
	s.ImagesReady = s.State == "launching" && s.HasImages
	return s, err
}

// Status returns lifecycle metadata, including for pre-API records.
func (l *Ledger) Status(ctx context.Context, id string) (Status, error) {
	return scanStatus(l.db.QueryRowContext(ctx, `SELECT `+statusColumns+` FROM stacks WHERE id = ?`, id))
}

// Statuses returns all saved stacks in ID order.
func (l *Ledger) Statuses(ctx context.Context) ([]Status, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT `+statusColumns+` FROM stacks ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var statuses []Status
	for rows.Next() {
		s, err := scanStatus(rows)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, s)
	}
	return statuses, rows.Err()
}

// Pending marks an imminent publication during launch or start.
func (l *Ledger) Pending(ctx context.Context, id string) error {
	return l.updateOne(ctx, `UPDATE stacks SET publication_pending = 1 WHERE id = ? AND api_owned = 1 AND state IN ('launching', 'starting')`, id)
}

// Ready commits the result and clears the publication marker in one update.
func (l *Ledger) Ready(ctx context.Context, id, url string, bindings any, now time.Time, duration time.Duration) error {
	data, err := json.Marshal(bindings)
	if err != nil {
		return err
	}
	return l.updateOne(ctx, `UPDATE stacks SET state = 'ready', url = ?, bindings = ?, ready_at = ?, expires_at = ?, publication_pending = 0 WHERE id = ? AND api_owned = 1 AND state IN ('launching', 'starting') AND publication_pending = 1`, url, data, now.UTC().Format(time.RFC3339Nano), now.Add(duration).UTC().Format(time.RFC3339Nano), id)
}

// Fail moves a launch to failed or a start to stopping for teardown.
// A false result means no transition occurred and the route must be left alone.
func (l *Ledger) Fail(ctx context.Context, id, phase, message string) (bool, error) {
	result, err := l.db.ExecContext(ctx, `UPDATE stacks SET state = CASE state WHEN 'starting' THEN 'stopping' ELSE 'failed' END, failure_phase = ?, failure = ? WHERE id = ? AND api_owned = 1 AND state IN ('launching', 'starting')`, phase, message, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// ClearPending records successful route fencing.
func (l *Ledger) ClearPending(ctx context.Context, id string) error {
	_, err := l.db.ExecContext(ctx, `UPDATE stacks SET publication_pending = 0 WHERE id = ? AND api_owned = 1 AND state IN ('failed', 'stopping', 'removing') AND publication_pending = 1`, id)
	return err
}
func (l *Ledger) updateOne(ctx context.Context, query string, args ...any) error {
	result, err := l.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("stack update affected %d rows", n)
	}
	return nil
}

// LaunchRecord contains the state needed to recover an API launch.
type LaunchRecord struct {
	ID, Hostname, State string
	Pending             bool
}

// APIRecords returns API launches relevant to recovery and ingress validation.
func (l *Ledger) APIRecords(ctx context.Context) ([]LaunchRecord, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT id, hostname, state, publication_pending FROM stacks
        WHERE api_owned = 1 AND (state IN ('launching', 'starting', 'ready', 'stopping') OR publication_pending = 1)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []LaunchRecord
	for rows.Next() {
		var record LaunchRecord
		if err := rows.Scan(&record.ID, &record.Hostname, &record.State, &record.Pending); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// Checkouts returns every recorded checkout, including legacy ownership.
func (l *Ledger) Checkouts(ctx context.Context) (map[string]bool, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT checkout FROM stacks WHERE checkout IS NOT NULL AND checkout <> ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		result[name] = true
	}
	return result, rows.Err()
}

// BeginStop makes the withdrawal retryable before any external calls.
func (l *Ledger) BeginStop(ctx context.Context, id string) error {
	return l.updateOne(ctx, `UPDATE stacks SET state = 'stopping', url = '', bindings = NULL, expires_at = '' WHERE id = ? AND api_owned = 1 AND state = 'ready'`, id)
}

// FinishStop records successful teardown.
func (l *Ledger) FinishStop(ctx context.Context, id string) error {
	return l.updateOne(ctx, `UPDATE stacks SET state = 'stopped', publication_pending = 0 WHERE id = ? AND api_owned = 1 AND state = 'stopping'`, id)
}

// Environment contains operator overrides, never admin defaults.
type Environment map[string]map[string]string

// GetEnvironment reads saved overrides, treating older records as empty.
func (l *Ledger) GetEnvironment(ctx context.Context, id string) (Environment, error) {
	var data []byte
	if err := l.db.QueryRowContext(ctx, `SELECT environment FROM stacks WHERE id = ?`, id).Scan(&data); err != nil {
		return nil, err
	}
	return decodeEnvironment(data)
}

func decodeEnvironment(data []byte) (Environment, error) {
	env := Environment{}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, err
		}
	}
	return env, nil
}

// SetEnvironment replaces a draft's overrides before it is claimed for launch.
func (l *Ledger) SetEnvironment(ctx context.Context, id string, env Environment) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return l.updateOne(ctx, `UPDATE stacks SET environment = ? WHERE id = ? AND api_owned = 1 AND state = 'draft'`, data, id)
}

// BeginStart claims a stopped API record and clears the previous run's metadata.
func (l *Ledger) BeginStart(ctx context.Context, id string) error {
	return l.updateOne(ctx, `UPDATE stacks SET state = 'starting', url = '', bindings = NULL, ready_at = '', expires_at = '', failure_phase = '', failure = '' WHERE id = ? AND api_owned = 1 AND state = 'stopped' AND publication_pending = 0`, id)
}

// ExpiryCandidate identifies a ready API stack and its saved deadline.
type ExpiryCandidate struct {
	ID, ExpiresAt string
}

// ExpiryCandidates returns ready API stacks with a recorded deadline.
func (l *Ledger) ExpiryCandidates(ctx context.Context) ([]ExpiryCandidate, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT id, expires_at FROM stacks WHERE api_owned = 1 AND state = 'ready' AND expires_at <> ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []ExpiryCandidate
	for rows.Next() {
		var s ExpiryCandidate
		if err := rows.Scan(&s.ID, &s.ExpiresAt); err != nil {
			return nil, err
		}
		records = append(records, s)
	}
	return records, rows.Err()
}

// BeginRemove durably claims removal and marks any route for fencing.
func (l *Ledger) BeginRemove(ctx context.Context, id string) error {
	return l.updateOne(ctx, `UPDATE stacks SET state = 'removing', publication_pending = CASE WHEN state IN ('ready', 'stopping') THEN 1 ELSE publication_pending END, url = '', bindings = NULL, expires_at = '' WHERE id = ? AND api_owned = 1 AND state IN ('draft', 'ready', 'stopping', 'stopped', 'failed', 'removing')`, id)
}

// RemovalAssets reports whether project setup was claimed and any exclusive checkout.
func (l *Ledger) RemovalAssets(ctx context.Context, id string) (bool, string, error) {
	var hasProject bool
	var checkout string
	err := l.db.QueryRowContext(ctx, `SELECT images IS NOT NULL, CASE WHEN EXISTS (
        SELECT 1 FROM stacks other WHERE other.id <> stacks.id AND other.checkout = stacks.checkout
    ) THEN '' ELSE COALESCE(checkout, '') END
    FROM stacks WHERE id = ? AND api_owned = 1 AND state = 'removing'`, id).Scan(&hasProject, &checkout)
	return hasProject, checkout, err
}

// FinishRemove deletes only a fenced removal record.
func (l *Ledger) FinishRemove(ctx context.Context, id string) error {
	return l.updateOne(ctx, `DELETE FROM stacks WHERE id = ? AND api_owned = 1 AND state = 'removing' AND publication_pending = 0`, id)
}
