package ledger

import (
	"context"
	"encoding/json"
	"time"
)

// CreateDraft reserves a run ID and stores an independent snapshot of its template.
func (l *Ledger) CreateDraft(ctx context.Context, id, name string, duration time.Duration, snapshot Template) error {
	env, err := json.Marshal(snapshot.Environment)
	if err != nil {
		return err
	}
	// The draft uses its ID as a placeholder until ingress assigns the hostname.
	return l.createNamedStack(ctx, id, `INSERT INTO stacks
		(id, hostname, duration_ns, state, api_owned, template_name, manifest, environment, repository, commit_hash, manifest_path, checkout)
		VALUES (?, ?, ?, 'draft', 1, ?, ?, ?, '', '', '', '') ON CONFLICT DO NOTHING`, id, id, int64(duration), name, snapshot.Content, env)
}

// SaveDraft updates a draft or claims it for launch with the supplied hostname.
func (l *Ledger) SaveDraft(ctx context.Context, id, host string, source Manifest, env Environment) error {
	environment, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if source.References == nil {
		source.References = map[string]ReferenceOverride{}
	}
	references, err := json.Marshal(source.References)
	if err != nil {
		return err
	}
	state := "launching"
	if host == "" {
		state, host = "draft", id
	}
	return l.updateOne(ctx, `UPDATE stacks SET manifest = ?, environment = ?, ref_overrides = ?, state = ?, hostname = ?
		WHERE id = ? AND api_owned = 1 AND state = 'draft'`, source.Content, environment, references, state, host, id)
}
