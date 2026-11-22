package httpserver

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"stack-launcher.local/app/internal/ledger"
)

func TestStackNamesAreDNSLabels(t *testing.T) {
	local := time.Date(2026, 11, 22, 1, 0, 0, 0, time.FixedZone("fixture", 5*60*60))
	pattern := regexp.MustCompile(`^[a-z]+-[a-z]+-20261121$`)
	seen := map[string]bool{}
	for index := range len(stackAdverbs) * len(stackNouns) {
		id := stackName(local, index)
		if !pattern.MatchString(id) || seen[id] || len(id) > stackNameLimit-2 {
			t.Fatalf("invalid or duplicate stack ID %q", id)
		}
		seen[id] = true
	}
	a, _ := setupAPI(t)
	id, host, err := a.reserveLaunch(t.Context(), local, time.Hour, nil, "", nil)
	if err != nil || !seen[id] || host != id+".example.test" {
		t.Fatalf("allocated %q at %q: %v", id, host, err)
	}
	a.cfg.BaseDomain = strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 63)
	if _, _, err := a.reserveLaunch(t.Context(), local, time.Hour, nil, "", nil); !errors.Is(err, errBaseDomainLength) {
		t.Fatalf("oversized hostname: %v", err)
	}
}

func TestStackNameClaimSurvivesRemoval(t *testing.T) {
	a, _ := setupAPI(t)
	ctx := t.Context()
	const id, host = "quietly-otter-20261121", "quietly-otter-20261121.example.test"
	if err := a.claimLaunch(ctx, id, host, time.Hour, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.claimLaunch(ctx, id, host, time.Hour, nil, "", nil); !errors.Is(err, ledger.ErrStackNameTaken) {
		t.Fatalf("concurrent claim: %v", err)
	}
	if changed, err := a.db.Fail(ctx, id, "startup", "fixture"); !changed || err != nil {
		t.Fatalf("fail launch: %t %v", changed, err)
	}
	if err := a.db.BeginRemove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := a.db.FinishRemove(ctx, id); err != nil {
		t.Fatal(err)
	}
	a.operations.Delete(id)
	if err := a.claimLaunch(ctx, id, host, time.Hour, nil, "", nil); !errors.Is(err, ledger.ErrStackNameTaken) {
		t.Fatalf("removed ID could be reused: %v", err)
	}
}
