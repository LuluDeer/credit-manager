package service

import (
	"context"
	"errors"
	"testing"

	"github.com/yuluo688/credit-manager/internal/store"
)

func tierCandidates() []AuthPickCandidate {
	return []AuthPickCandidate{
		{ID: "low-1", Provider: "codex", Priority: 0},
		{ID: "high-1", Provider: "codex", Priority: 10},
		{ID: "high-2", Provider: "codex", Priority: 10},
		{ID: "low-2", Provider: "codex", Priority: 0},
	}
}

func occupy(svc *Service, id, authID string) {
	svc.authMu.Lock()
	svc.ensureAuthPendingLocked()
	svc.authPending[id] = &pendingAuthCapture{reservationID: id, active: true, hasAuth: true, auth: store.AuthIdentity{AuthID: authID, Provider: "codex"}}
	svc.authMu.Unlock()
}

func TestPickStaysOnTopTier(t *testing.T) {
	svc := openTestService(t)
	ctx := context.Background()
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		id, handled, err := svc.PickAuthForRequest(ctx, "m", "", "", tierCandidates())
		if err != nil || !handled {
			t.Fatal(err)
		}
		seen[id]++
	}
	if seen["high-1"] != 3 || seen["high-2"] != 3 || len(seen) != 2 {
		t.Fatalf("round-robin must stay on top tier: %v", seen)
	}
}

func TestPickFallsToLowerTierWhenTopFull(t *testing.T) {
	svc := openTestService(t)
	ctx := context.Background()
	for _, id := range []string{"high-1", "high-2", "low-1"} {
		if _, err := svc.SetAuthConcurrencyLimit(ctx, "codex", id, 1); err != nil {
			t.Fatal(err)
		}
	}
	occupy(svc, "r1", "high-1")
	occupy(svc, "r2", "high-2")
	id, _, err := svc.PickAuthForRequest(ctx, "m", "", "", tierCandidates())
	if err != nil || (id != "low-1" && id != "low-2") {
		t.Fatalf("want lower tier, got %q %v", id, err)
	}
	// Top tier frees up -> back to it.
	svc.authMu.Lock()
	delete(svc.authPending, "r1")
	svc.authMu.Unlock()
	if id, _, _ := svc.PickAuthForRequest(ctx, "m", "", "", tierCandidates()); id != "high-1" {
		t.Fatalf("want high-1 after it frees, got %q", id)
	}
	// Everything full -> reject.
	svc.authMu.Lock()
	svc.authPending = map[string]*pendingAuthCapture{}
	svc.authMu.Unlock()
	if _, err := svc.SetAuthConcurrencyLimit(ctx, "codex", "low-2", 1); err != nil {
		t.Fatal(err)
	}
	for i, a := range []string{"high-1", "high-2", "low-1", "low-2"} {
		occupy(svc, "x"+string(rune('0'+i)), a)
	}
	if _, handled, err := svc.PickAuthForRequest(ctx, "m", "", "", tierCandidates()); !handled || !errors.Is(err, store.ErrConcurrentLimit) {
		t.Fatalf("all full should reject: %v %v", handled, err)
	}
}

func TestPickFiltersDisabledCandidates(t *testing.T) {
	svc := openTestService(t)
	c := []AuthPickCandidate{{ID: "high-1", Provider: "codex", Priority: 10, Status: "disabled"}, {ID: "low-1", Provider: "codex", Priority: 0, Status: "active"}}
	if id, _, err := svc.PickAuthForRequest(context.Background(), "m", "", "", c); err != nil || id != "low-1" {
		t.Fatalf("disabled must be skipped: %q %v", id, err)
	}
}

func TestAffinityBeatsPriority(t *testing.T) {
	svc := openTestService(t)
	ctx := context.Background()
	if _, err := svc.UpdateAuthRoutingSettings(ctx, store.AuthRoutingSettings{Strategy: store.AuthRoutingRoundRobin, SessionAffinity: true, SessionAffinityTTLSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	// Bind the session while only the low tier is offered.
	low := []AuthPickCandidate{{ID: "low-1", Provider: "codex", Priority: 0}}
	if id, _, _ := svc.PickAuthForRequest(ctx, "m", "sess", "", low); id != "low-1" {
		t.Fatalf("bind: %q", id)
	}
	// Higher tier appears: the bound, available account still wins.
	if id, _, _ := svc.PickAuthForRequest(ctx, "m", "sess", "", tierCandidates()); id != "low-1" {
		t.Fatalf("affinity should beat priority, got %q", id)
	}
	// Bound account becomes full: rebind via tiered pick (top tier).
	if _, err := svc.SetAuthConcurrencyLimit(ctx, "codex", "low-1", 1); err != nil {
		t.Fatal(err)
	}
	occupy(svc, "busy", "low-1")
	id, _, _ := svc.PickAuthForRequest(ctx, "m", "sess", "", tierCandidates())
	if id != "high-1" && id != "high-2" {
		t.Fatalf("rebind should use top tier, got %q", id)
	}
	svc.authMu.Lock()
	delete(svc.authPending, "busy")
	svc.authMu.Unlock()
	if again, _, _ := svc.PickAuthForRequest(ctx, "m", "sess", "", tierCandidates()); again != id {
		t.Fatalf("session should stay on rebound %q, got %q", id, again)
	}
}
