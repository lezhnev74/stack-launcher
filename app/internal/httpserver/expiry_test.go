package httpserver

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/podman"
)

type expiryTestRouter struct {
	*fakeRouter
	withdraw func(string) error
}

func (r expiryTestRouter) Withdraw(_ context.Context, host string) error { return r.withdraw(host) }

func saveExpiryStack(t *testing.T, db *ledger.Ledger, id string, ready time.Time, duration time.Duration) {
	t.Helper()
	ctx, host := t.Context(), id+".example.test"
	if err := db.CreateAPI(ctx, id, host, duration); err != nil {
		t.Fatal(err)
	}
	if err := db.Pending(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := db.Ready(ctx, id, "http://"+host, nil, ready, duration); err != nil {
		t.Fatal(err)
	}
}

func TestExpiryDeadline(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		deadline string
		want     bool
	}{
		{now.Add(time.Second).Format(time.RFC3339Nano), false},
		{now.Format(time.RFC3339Nano), true},
		{now.Add(-time.Second).Format(time.RFC3339Nano), true},
		{"", false}, {"invalid", false},
	} {
		if got := expired(tc.deadline, now); got != tc.want {
			t.Fatalf("%q: %v", tc.deadline, got)
		}
	}
}

func TestExpiryWorkerAndManualRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, base := setupAPI(t)
		var caddyFailure atomic.Bool
		caddyFailure.Store(true)
		a.router = expiryTestRouter{fakeRouter: base, withdraw: func(string) error {
			if caddyFailure.Load() {
				return errors.New("caddy")
			}
			return nil
		}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		saveExpiryStack(t, a.db, "old", time.Now(), expiryInterval)
		var calls atomic.Int32
		a.stop = func(context.Context, config.Config, *ledger.Ledger, string) error { calls.Add(1); return nil }
		go a.expiryLoop(ctx)
		synctest.Wait()
		s, err := a.db.Status(ctx, "old")
		if err != nil || s.State != "ready" {
			t.Fatalf("future deadline: %+v %v", s, err)
		}
		time.Sleep(expiryInterval)
		synctest.Wait()
		s, err = a.db.Status(ctx, "old")
		if err != nil || s.State != "stopping" || calls.Load() != 0 {
			t.Fatalf("failed withdrawal: %+v %v, stops %d", s, err, calls.Load())
		}
		caddyFailure.Store(false)
		time.Sleep(expiryInterval)
		synctest.Wait()
		s, err = a.db.Status(ctx, "old")
		if err != nil || s.State != "stopping" || calls.Load() != 0 {
			t.Fatalf("later expiry check: %+v %v, stops %d", s, err, calls.Load())
		}

		if w := serve(a, http.MethodPost, "/stacks/old/stop", ""); w.Code != 204 {
			t.Fatalf("retry: %d %s", w.Code, w.Body)
		}
		if calls.Load() != 1 {
			t.Fatalf("stops: %d", calls.Load())
		}
		cancel()
		synctest.Wait()
	})
}

func TestExpiryAtStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, _ := setupAPI(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		saveExpiryStack(t, a.db, "old", time.Now().Add(-2*time.Hour), time.Hour)
		done := make(chan struct{})
		a.stop = func(context.Context, config.Config, *ledger.Ledger, string) error { close(done); return nil }
		go a.expiryLoop(ctx)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("startup expiry did not stop stack")
		}
		s, err := a.db.Status(ctx, "old")
		if err != nil || s.State != "stopped" {
			t.Fatalf("status: %+v %v", s, err)
		}
		cancel()
		synctest.Wait()
	})
}

func TestExpiryRechecksCandidateAfterStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, _ := setupAPI(t)
		ctx := t.Context()
		saveExpiryStack(t, a.db, "old", time.Now().Add(-time.Hour), time.Minute)
		candidates, err := a.db.ExpiryCandidates(ctx)
		if err != nil || len(candidates) != 1 || !expired(candidates[0].ExpiresAt, time.Now()) {
			t.Fatalf("expiry candidates: %+v %v", candidates, err)
		}
		var stops atomic.Int32
		a.stop = func(context.Context, config.Config, *ledger.Ledger, string) error { stops.Add(1); return nil }
		a.start = func(context.Context, config.Config, *ledger.Ledger, string) (podman.Result, error) {
			return podman.Result{HTTPS: true}, nil
		}
		if w := serve(a, http.MethodPost, "/stacks/old/stop", ""); w.Code != 204 {
			t.Fatalf("stop: %d %s", w.Code, w.Body)
		}
		if w := serve(a, http.MethodPost, "/stacks/old/start", ""); w.Code != 202 {
			t.Fatalf("start: %d %s", w.Code, w.Body)
		}
		synctest.Wait()
		a.expireStack(ctx, candidates[0].ID)
		synctest.Wait()
		s, err := a.db.Status(ctx, "old")
		if err != nil || s.State != "ready" || expired(s.ExpiresAt, time.Now()) || stops.Load() != 1 {
			t.Fatalf("restarted stack: %+v %v, stops %d", s, err, stops.Load())
		}
	})
}

func TestExpirySerializesWithdrawalsAndStopsProjectsIndependently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, base := setupAPI(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		ready := time.Now().Add(-time.Hour)
		saveExpiryStack(t, a.db, "one", ready, time.Minute)
		saveExpiryStack(t, a.db, "two", ready, time.Minute)

		withdrawals, stops := make(chan string, 2), make(chan string, 2)
		allowWithdrawal, allowStop := make(chan struct{}, 2), make(chan struct{})
		defer close(allowWithdrawal)
		defer close(allowStop)
		a.router = expiryTestRouter{fakeRouter: base, withdraw: func(host string) error {
			withdrawals <- host
			<-allowWithdrawal
			return nil
		}}
		a.stop = func(_ context.Context, _ config.Config, _ *ledger.Ledger, id string) error {
			stops <- id
			<-allowStop
			return nil
		}
		go a.expiryLoop(ctx)
		synctest.Wait()
		if len(withdrawals) != 1 || len(stops) != 0 {
			t.Fatalf("before first withdrawal completes: %d withdrawals, %d stops", len(withdrawals), len(stops))
		}
		allowWithdrawal <- struct{}{}
		synctest.Wait()
		if len(withdrawals) != 2 || len(stops) != 1 {
			t.Fatalf("during first project teardown: %d withdrawals, %d stops", len(withdrawals), len(stops))
		}
		allowWithdrawal <- struct{}{}
		synctest.Wait()
		if len(stops) != 2 {
			t.Fatalf("both projects should be stopping independently, got %d stops", len(stops))
		}
	})
}
