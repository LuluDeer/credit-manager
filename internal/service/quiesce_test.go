package service

import (
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/store"
)

func TestBindPickLockedPrefersNamedReservation(t *testing.T) {
	svc := openTestService(t)
	now := time.Now()
	svc.authMu.Lock()
	svc.authPending["old"] = &pendingAuthCapture{reservationID: "old", active: true, startedAt: now.Add(-time.Minute)}
	svc.authPending["new"] = &pendingAuthCapture{reservationID: "new", active: true, startedAt: now}
	svc.bindPickLocked("new", store.AuthIdentity{AuthID: "a1", Provider: "codex"})
	svc.bindPickLocked("missing", store.AuthIdentity{AuthID: "a2", Provider: "codex"})
	oldP, newP := svc.authPending["old"], svc.authPending["new"]
	svc.authMu.Unlock()
	if !newP.hasAuth || newP.auth.AuthID != "a1" {
		t.Fatalf("named reservation not bound: %+v", newP)
	}
	// A named-but-unknown reservation (already finished) must not steal
	// another request's slot; only header-less picks use the fallback.
	if oldP.hasAuth {
		t.Fatalf("unknown reservation leaked onto oldest: %+v", oldP)
	}
	svc.authMu.Lock()
	svc.bindPickLocked("", store.AuthIdentity{AuthID: "a3", Provider: "codex"})
	svc.authMu.Unlock()
	if !oldP.hasAuth || oldP.auth.AuthID != "a3" {
		t.Fatalf("header-less pick should bind oldest unattributed: %+v", oldP)
	}
}

func TestQuiesceIdempotent(t *testing.T) {
	Quiesce() // no current service: no-op
	svc := openTestService(t)
	Replace(svc)
	QuiesceDrainTimeout = 100 * time.Millisecond
	svc.authMu.Lock()
	svc.authPending["r"] = &pendingAuthCapture{reservationID: "r", active: true}
	svc.authMu.Unlock()
	start := time.Now()
	Quiesce()
	if Current() != nil {
		t.Fatal("service should be released after quiesce")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("drain did not respect timeout")
	}
	Quiesce()
}
