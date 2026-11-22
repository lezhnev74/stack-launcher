package ledger

import (
	"context"
	"time"
)

// LaunchActivity is a timestamped startup milestone for a run.
type LaunchActivity struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

// RecordLaunchActivity appends a milestone to the run's history.
func (l *Ledger) RecordLaunchActivity(ctx context.Context, id, message string) error {
	_, err := l.db.ExecContext(ctx, `INSERT INTO stack_launch_activity (stack_id, recorded_at, message) VALUES (?, ?, ?)`,
		id, time.Now().UTC().Format(time.RFC3339Nano), message)
	return err
}

// LaunchHistory returns the run's recorded milestones in the order they occurred.
func (l *Ledger) LaunchHistory(ctx context.Context, id string) ([]LaunchActivity, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT recorded_at, message FROM stack_launch_activity WHERE stack_id = ? ORDER BY sequence`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var events []LaunchActivity
	for rows.Next() {
		var event LaunchActivity
		var recordedAt string
		if err := rows.Scan(&recordedAt, &event.Message); err != nil {
			return nil, err
		}
		event.Time, err = time.Parse(time.RFC3339Nano, recordedAt)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
