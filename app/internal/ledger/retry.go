package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
)

// LaunchRequest retains the original source until acquisition and image overrides until retry.
type LaunchRequest struct {
	Reference  string            `json:"reference,omitempty"`
	InlineYAML []byte            `json:"inline_yaml"`
	Images     map[string]string `json:"images,omitempty"`
}

const retryInputs = `(launch_request IS NOT NULL OR (manifest IS NOT NULL AND (ref_overrides IS NOT NULL OR images IS NOT NULL)))`

func encodeLaunchRequest(request *LaunchRequest) (any, error) {
	if request == nil {
		return nil, nil
	}
	data, err := json.Marshal(request)
	return string(data), err
}

// LaunchRequest reads the launch input, treating records without it as having no overrides.
func (l *Ledger) LaunchRequest(ctx context.Context, id string) (LaunchRequest, error) {
	var data sql.NullString
	var request LaunchRequest
	if err := l.db.QueryRowContext(ctx, `SELECT launch_request FROM stacks WHERE id = ?`, id).Scan(&data); err != nil {
		return request, err
	}
	if !data.Valid {
		return request, nil
	}
	return request, json.Unmarshal([]byte(data.String), &request)
}

// BeginRetry claims a failed run after any previous publication has been fenced.
func (l *Ledger) BeginRetry(ctx context.Context, id string) error {
	return l.updateOne(ctx, `UPDATE stacks SET state = 'launching', url = '', bindings = NULL, ready_at = '',
		expires_at = '', failure_phase = '', failure = ''
		WHERE id = ? AND api_owned = 1 AND state = 'failed' AND publication_pending = 0 AND `+retryInputs, id)
}
