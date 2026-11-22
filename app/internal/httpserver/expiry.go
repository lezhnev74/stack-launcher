package httpserver

import (
	"context"
	"log"
	"time"

	"stack-launcher.local/app/internal/runlog"
)

const expiryInterval = 5 * time.Second

func (a *api) expiryLoop(ctx context.Context) {
	ticker := time.NewTicker(expiryInterval)
	defer ticker.Stop()
	a.expire(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.expire(ctx, time.Now())
		}
	}
}

func expired(expiresAt string, now time.Time) bool {
	deadline, err := time.Parse(time.RFC3339Nano, expiresAt)
	return err == nil && !deadline.After(now)
}

func (a *api) expire(ctx context.Context, now time.Time) {
	records, err := a.db.ExpiryCandidates(ctx)
	if err != nil {
		log.Printf("read expiry candidates: %v", err)
		return
	}
	for _, s := range records {
		if ctx.Err() != nil {
			return
		}
		if expired(s.ExpiresAt, now) {
			a.expireStack(ctx, s.ID)
		}
	}
}

func (a *api) expireStack(ctx context.Context, id string) {
	if _, loaded := a.operations.LoadOrStore(id, struct{}{}); loaded {
		return
	}
	if !a.withdrawExpiredStack(ctx, id) {
		a.operations.Delete(id)
		return
	}
	// Expiry withdrawals update one shared Caddy route list in sequence.
	// Podman teardown can run independently for each withdrawn stack.
	go func() {
		defer a.operations.Delete(id)
		_, _ = a.stopProject(ctx, id)
	}()
}

func (a *api) withdrawExpiredStack(ctx context.Context, id string) bool {
	s, err := a.db.Status(ctx, id)
	if err != nil {
		runlog.Printf(id, "Could not read run status for expiry error=%q", err)
		return false
	}
	if s.State != "ready" || !expired(s.ExpiresAt, time.Now()) {
		return false
	}
	if _, message := a.stopEligibility(s); message != "" {
		runlog.Printf(id, "Expiry stop rejected reason=%q", message)
		return false
	}
	runlog.Printf(id, "Stop triggered by expiry expires_at=%q", s.ExpiresAt)
	code, _ := a.withdrawStack(ctx, s)
	return code == 0
}
