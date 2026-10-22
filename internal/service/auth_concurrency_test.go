package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yuluo688/credit-manager/internal/store"
)

func TestSetAuthConcurrencyLimitRoundTrip(t *testing.T) {
	s := quotaService(t)
	s.SetAuthQuotaSource(&fakeQuotaSource{files: []AuthQuotaFile{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex", Label: "ops"}}, auth: quotaJSON("codex")})
	item, err := s.SetAuthConcurrencyLimit(context.Background(), "codex", "auth-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if item.MaxConcurrentRequests != 2 || item.AuthID != "auth-1" {
		t.Fatalf("item = %#v", item)
	}
	listed, err := s.AuthQuotaOverview(context.Background(), "", AuthQuotaFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 1 || listed.Items[0].MaxConcurrentRequests != 2 {
		t.Fatalf("overview = %#v", listed.Items)
	}
}

func TestSetAuthConcurrencyLimitsBatch(t *testing.T) {
	s := quotaService(t)
	s.SetAuthQuotaSource(&fakeQuotaSource{files: []AuthQuotaFile{
		{ID: "auth-1", Provider: "codex", Label: "ops"},
		{ID: "auth-2", Provider: "codex", Label: "bot"},
		{ID: "auth-3", Provider: "xai", Label: "grok"},
	}, auth: quotaJSON("codex")})
	got, err := s.SetAuthConcurrencyLimits(context.Background(), AuthQuotaFilter{Provider: "codex"}, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got.Updated != 2 || got.MaxConcurrentRequests != 3 {
		t.Fatalf("filter batch = %#v", got)
	}
	page, err := s.SetAuthConcurrencyLimits(context.Background(), AuthQuotaFilter{}, []AuthConcurrencyTarget{{Provider: "xai", AuthID: "auth-3"}}, 1)
	if err != nil || page.Updated != 1 {
		t.Fatalf("item batch = %#v, %v", page, err)
	}
	listed, err := s.AuthQuotaOverview(context.Background(), "", AuthQuotaFilter{PageSize: 24})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]int64{}
	for _, item := range listed.Items {
		byID[item.AuthID] = item.MaxConcurrentRequests
	}
	if byID["auth-1"] != 3 || byID["auth-2"] != 3 || byID["auth-3"] != 1 {
		t.Fatalf("limits = %#v", byID)
	}
}

func TestAdmitAuthEnforcesConcurrencyLimit(t *testing.T) {
	s := quotaService(t)
	if err := s.Store().UpsertAuthConcurrencyLimit(context.Background(), "claude", "auth-1", 1); err != nil {
		t.Fatal(err)
	}
	s.TrackAuthCapture("res-1", "claude-sonnet")
	s.TrackAuthCapture("res-2", "claude-sonnet")
	auth := store.AuthIdentity{AuthID: "auth-1", Provider: "claude"}
	if err := s.AdmitAuth(context.Background(), "res-1", auth); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if err := s.AdmitAuth(context.Background(), "res-2", auth); !errors.Is(err, store.ErrConcurrentLimit) {
		t.Fatalf("second admit error = %v", err)
	}
	s.CancelAuthCapture("res-1")
	if err := s.AdmitAuth(context.Background(), "res-2", auth); err != nil {
		t.Fatalf("admit after release: %v", err)
	}
}

func TestFinishAuthCaptureReleasesConcurrencyLimit(t *testing.T) {
	s := quotaService(t)
	if err := s.Store().UpsertAuthConcurrencyLimit(context.Background(), "claude", "auth-1", 1); err != nil {
		t.Fatal(err)
	}
	s.TrackAuthCapture("res-finished", "claude-sonnet")
	s.TrackAuthCapture("res-active", "claude-sonnet")
	auth := store.AuthIdentity{AuthID: "auth-1", Provider: "claude"}
	if err := s.AdmitAuth(context.Background(), "res-finished", auth); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	s.FinishAuthCapture("res-finished")
	if err := s.AdmitAuth(context.Background(), "res-active", auth); err != nil {
		t.Fatalf("admit after execution complete: %v", err)
	}
}

func TestPickAuthSkipsFinishedCapture(t *testing.T) {
	s := quotaService(t)
	if err := s.Store().UpsertAuthConcurrencyLimit(context.Background(), "claude", "auth-1", 2); err != nil {
		t.Fatal(err)
	}
	s.TrackAuthCapture("res-finished", "claude-sonnet")
	s.FinishAuthCapture("res-finished")
	s.TrackAuthCapture("res-active", "claude-sonnet")
	if _, handled, err := s.PickAuth(context.Background(), []AuthPickCandidate{{ID: "auth-1", Provider: "claude"}}); err != nil || !handled {
		t.Fatalf("pick = (%t, %v)", handled, err)
	}
	s.authMu.Lock()
	finished := s.authPending["res-finished"]
	active := s.authPending["res-active"]
	s.authMu.Unlock()
	if finished == nil || finished.hasAuth || active == nil || !active.hasAuth {
		t.Fatalf("auth bindings = finished:%#v active:%#v", finished, active)
	}
}

func TestPickAuthSkipsBusyAndFallsBackWhenUnlimited(t *testing.T) {
	s := quotaService(t)
	candidates := []AuthPickCandidate{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}}
	id, handled, err := s.PickAuth(context.Background(), candidates)
	if err != nil || !handled || id == "" {
		t.Fatalf("unlimited pick = (%q, %t, %v)", id, handled, err)
	}
	if err := s.Store().UpsertAuthConcurrencyLimit(context.Background(), "codex", "a", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Store().UpsertAuthConcurrencyLimit(context.Background(), "codex", "b", 1); err != nil {
		t.Fatal(err)
	}
	s.TrackAuthCapture("res-a", "gpt")
	if err := s.AdmitAuth(context.Background(), "res-a", store.AuthIdentity{AuthID: "a", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	id, handled, err = s.PickAuth(context.Background(), candidates)
	if err != nil || !handled || id != "b" {
		t.Fatalf("busy skip = (%q, %t, %v)", id, handled, err)
	}
	s.TrackAuthCapture("res-b", "gpt")
	if err := s.AdmitAuth(context.Background(), "res-b", store.AuthIdentity{AuthID: "b", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	if _, handled, err = s.PickAuth(context.Background(), candidates); !handled || !errors.Is(err, store.ErrConcurrentLimit) {
		t.Fatalf("all busy = handled=%t err=%v", handled, err)
	}
}

func TestPickAuthBindsOldestUnattributedReservation(t *testing.T) {
	s := quotaService(t)
	if err := s.Store().UpsertAuthConcurrencyLimit(context.Background(), "claude", "auth-1", 3); err != nil {
		t.Fatal(err)
	}
	s.TrackAuthCapture("res-1", "claude-sonnet")
	id, handled, err := s.PickAuth(context.Background(), []AuthPickCandidate{{ID: "auth-1", Provider: "claude"}})
	if err != nil || !handled || id != "auth-1" {
		t.Fatalf("pick = (%q, %t, %v)", id, handled, err)
	}
	s.authMu.Lock()
	got := s.activeAuthRequestsLocked("claude", "auth-1", "")
	s.authMu.Unlock()
	if got != 1 {
		t.Fatalf("active = %d", got)
	}
}

func TestAuthWarmupHoldExcludesNormalSchedulerSelection(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	warmupAuth := store.AuthIdentity{AuthID: "warmup", Provider: "codex"}
	if err := s.AcquireAuthWarmup(ctx, "run-1", warmupAuth); err != nil {
		t.Fatal(err)
	}
	defer s.ReleaseAuthWarmup("run-1")
	if _, handled, err := s.PickAuth(ctx, []AuthPickCandidate{{ID: "warmup", Provider: "codex"}}); !handled || !errors.Is(err, store.ErrConcurrentLimit) {
		t.Fatalf("busy warmup target pick = handled:%t err:%v", handled, err)
	}
	if id, handled, err := s.PickAuth(ctx, []AuthPickCandidate{{ID: "warmup", Provider: "codex"}, {ID: "other", Provider: "codex"}}); err != nil || !handled || id != "other" {
		t.Fatalf("fallback around warmup = (%q, %t, %v)", id, handled, err)
	}
}

func TestPickAuthAttributesInFlightWithoutLimits(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	active := func(id string) int64 {
		s.authMu.Lock()
		defer s.authMu.Unlock()
		return s.activeAuthRequestsLocked("codex", id, "")
	}
	candidates := []AuthPickCandidate{{ID: "b", Provider: "codex"}, {ID: "a", Provider: "codex"}, {ID: "c", Provider: "codex"}}
	got := []string{}
	for _, res := range []string{"r1", "r2", "r3"} {
		s.TrackAuthCapture(res, "gpt")
		id, handled, err := s.PickAuthForModel(ctx, "gpt", candidates)
		if err != nil || !handled {
			t.Fatalf("pick = (%q, %t, %v)", id, handled, err)
		}
		got = append(got, id)
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("round-robin order = %v, want ID-sorted a,b,c", got)
	}
	for _, id := range []string{"a", "b", "c"} {
		if active(id) != 1 {
			t.Fatalf("active(%s) = %d, want 1", id, active(id))
		}
	}
	for _, res := range []string{"r1", "r2", "r3"} {
		s.FinishAuthCapture(res)
	}
	if active("a")+active("b")+active("c") != 0 {
		t.Fatal("in-flight not released after finish")
	}
}

func TestPickAuthRoutingStrategies(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	candidates := []AuthPickCandidate{{ID: "b", Provider: "codex"}, {ID: "a", Provider: "codex"}}
	if _, err := s.UpdateAuthRoutingSettings(ctx, store.AuthRoutingSettings{Strategy: "bogus"}); err == nil {
		t.Fatal("expected invalid strategy error")
	}
	if _, err := s.UpdateAuthRoutingSettings(ctx, store.AuthRoutingSettings{Strategy: "fill-first"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s.TrackAuthCapture(fmt.Sprintf("ff-%d", i), "gpt")
		if id, _, err := s.PickAuthForModel(ctx, "gpt", candidates); err != nil || id != "a" {
			t.Fatalf("fill-first = %q %v", id, err)
		}
	}
	// a has 2 in flight; limit a to 2 -> fill-first spills to b.
	if err := s.Store().UpsertAuthConcurrencyLimit(ctx, "codex", "a", 2); err != nil {
		t.Fatal(err)
	}
	s.TrackAuthCapture("ff-2", "gpt")
	if id, _, err := s.PickAuthForModel(ctx, "gpt", candidates); err != nil || id != "b" {
		t.Fatalf("fill-first spill = %q %v", id, err)
	}
	if _, err := s.UpdateAuthRoutingSettings(ctx, store.AuthRoutingSettings{Strategy: "least-busy"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store().UpsertAuthConcurrencyLimit(ctx, "codex", "a", 0); err != nil {
		t.Fatal(err)
	}
	s.TrackAuthCapture("lb-0", "gpt")
	if id, _, err := s.PickAuthForModel(ctx, "gpt", candidates); err != nil || id != "b" {
		t.Fatalf("least-busy = %q %v (a=2,b=1 in flight)", id, err)
	}
	settings, err := s.AuthRoutingSettings(ctx)
	if err != nil || settings.Strategy != "least-busy" || settings.UpdatedAt == nil {
		t.Fatalf("settings = %#v %v", settings, err)
	}
}

func TestPickAuthSessionAffinity(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	if _, err := s.UpdateAuthRoutingSettings(ctx, store.AuthRoutingSettings{Strategy: "round-robin", SessionAffinity: true}); err != nil {
		t.Fatal(err)
	}
	candidates := []AuthPickCandidate{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}, {ID: "c", Provider: "codex"}}
	pick := func(session string) string {
		id, handled, err := s.PickAuthForSession(ctx, "gpt", session, candidates)
		if err != nil || !handled {
			t.Fatalf("pick = (%q, %t, %v)", id, handled, err)
		}
		return id
	}
	first := pick("s1")
	for i := 0; i < 4; i++ {
		if got := pick("s1"); got != first {
			t.Fatalf("sticky session moved %s -> %s", first, got)
		}
	}
	if pick("s2") == pick("s3") {
		t.Fatal("different sessions should spread under round-robin")
	}
	// Bound auth full -> fall back and rebind.
	if err := s.Store().UpsertAuthConcurrencyLimit(ctx, "codex", first, 1); err != nil {
		t.Fatal(err)
	}
	s.TrackAuthCapture("busy", "gpt")
	if err := s.AdmitAuth(ctx, "busy", store.AuthIdentity{AuthID: first, Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	moved := pick("s1")
	if moved == first {
		t.Fatal("full bound auth should not be reused")
	}
	s.FinishAuthCapture("busy")
	if got := pick("s1"); got != moved {
		t.Fatalf("session should stay on rebound auth %s, got %s", moved, got)
	}
	// Disabled affinity -> plain round-robin.
	if _, err := s.UpdateAuthRoutingSettings(ctx, store.AuthRoutingSettings{Strategy: "round-robin"}); err != nil {
		t.Fatal(err)
	}
	if pick("s1") == pick("s1") && pick("s1") == pick("s1") {
		t.Fatal("affinity off should rotate")
	}
}

// Mirrors CPA TestRoundRobinSelector successor semantics: rotation resumes at
// the auth after the last pick even when candidates drop out and return.
func TestPickAuthRoundRobinMatchesCPASuccessor(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	all := []AuthPickCandidate{{ID: "c", Provider: "codex"}, {ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}}
	pick := func(model string, cands []AuthPickCandidate) string {
		id, _, err := s.PickAuthForModel(ctx, model, cands)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	var got []string
	for i := 0; i < 4; i++ {
		got = append(got, pick("gpt", all))
	}
	if fmt.Sprint(got) != "[a b c a]" {
		t.Fatalf("rotation = %v", got)
	}
	// last = a; b drops out -> c (not a repeat, not a skip of c).
	if id := pick("gpt", []AuthPickCandidate{{ID: "a", Provider: "codex"}, {ID: "c", Provider: "codex"}}); id != "c" {
		t.Fatalf("after drop = %s want c", id)
	}
	// b returns; last = c -> wraps to a, then b.
	if id := pick("gpt", all); id != "a" {
		t.Fatalf("wrap = %s want a", id)
	}
	if id := pick("gpt", all); id != "b" {
		t.Fatalf("returned = %s want b", id)
	}
	// Thinking suffix shares the rotation key: last = b -> c.
	if id := pick("gpt(high)", all); id != "c" {
		t.Fatalf("suffix key = %s want c", id)
	}
	// Different model has its own key.
	if id := pick("other", all); id != "a" {
		t.Fatalf("other model = %s want a", id)
	}
}

func TestCanonicalModelKeyMatchesCPA(t *testing.T) {
	for in, want := range map[string]string{
		"gpt-5.6-luna": "gpt-5.6-luna", "gpt-5.6-luna(high)": "gpt-5.6-luna", "claude(16384)": "claude",
		" m(x) ": "m", "m(x": "m(x", "(x)": "(x)", "a(b)(c)": "a(b)", "": "",
	} {
		if got := canonicalModelKey(in); got != want {
			t.Errorf("canonicalModelKey(%q) = %q want %q", in, got, want)
		}
	}
}

func TestPickAuthSessionAffinityThinkingSuffixAndFallbackKey(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	if _, err := s.UpdateAuthRoutingSettings(ctx, store.AuthRoutingSettings{Strategy: "round-robin", SessionAffinity: true}); err != nil {
		t.Fatal(err)
	}
	cands := []AuthPickCandidate{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}, {ID: "c", Provider: "codex"}}
	pick := func(model, session string) string {
		id, _, err := s.PickAuthForSession(ctx, model, session, cands)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	pick("gpt", "warm") // advance rotation so a fresh binding would differ
	first := pick("gpt", "short")
	for _, m := range []string{"gpt(high)", "gpt(low)", "gpt(8192)"} {
		if got := pick(m, "short"); got != first {
			t.Fatalf("%s moved %s -> %s", m, first, got)
		}
	}
	// Turn 2: primary full hash is new, fallback short hash hits the binding.
	if got := pick("gpt", "full,short"); got != first {
		t.Fatalf("fallback key moved %s -> %s", first, got)
	}
	// Full hash is now bound itself.
	if got := pick("gpt(high)", "full"); got != first {
		t.Fatalf("primary rebinding missing: %s -> %s", first, got)
	}
}

func TestPickAuthRetryRebindsSameReservation(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	active := func(id string) int64 {
		s.authMu.Lock()
		defer s.authMu.Unlock()
		return s.activeAuthRequestsLocked("codex", id, "")
	}
	s.TrackAuthCapture("res-a", "gpt")
	s.TrackAuthCapture("res-b", "gpt") // unrelated, still unattributed
	if id, _, err := s.PickAuthForRequest(ctx, "gpt", "", "res-a", []AuthPickCandidate{{ID: "yuluo", Provider: "codex"}}); err != nil || id != "yuluo" {
		t.Fatalf("first pick = %q %v", id, err)
	}
	// CPA retries the same request on another auth after a 429.
	if id, _, err := s.PickAuthForRequest(ctx, "gpt", "", "res-a", []AuthPickCandidate{{ID: "other", Provider: "codex"}}); err != nil || id != "other" {
		t.Fatalf("retry pick = %q %v", id, err)
	}
	if active("yuluo") != 0 || active("other") != 1 {
		t.Fatalf("active yuluo=%d other=%d, want 0/1", active("yuluo"), active("other"))
	}
	s.authMu.Lock()
	resB := s.authPending["res-b"]
	s.authMu.Unlock()
	if resB.hasAuth {
		t.Fatal("retry pick leaked onto unrelated reservation")
	}
	if got := s.AuthForSettlement("res-a", ""); got.AuthID != "other" {
		t.Fatalf("settlement auth = %q, want other", got.AuthID)
	}
	s.FinishAuthCapture("res-a")
	s.CancelAuthCapture("res-b")
	if active("yuluo")+active("other") != 0 {
		t.Fatal("in-flight not released")
	}
}

func TestPickAuthUnknownReservationDoesNotStealPending(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	s.TrackAuthCapture("res-live", "gpt")
	if _, _, err := s.PickAuthForRequest(ctx, "gpt", "", "res-gone", []AuthPickCandidate{{ID: "a", Provider: "codex"}}); err != nil {
		t.Fatal(err)
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.authPending["res-live"].hasAuth {
		t.Fatal("pick for finished reservation attributed to another request")
	}
}

func TestPickAuthNoUsableCandidates(t *testing.T) {
	s := quotaService(t)
	ctx := context.Background()
	if _, handled, err := s.PickAuth(ctx, nil); handled || err != nil {
		t.Fatalf("empty = %t %v", handled, err)
	}
	if _, handled, err := s.PickAuth(ctx, []AuthPickCandidate{{ID: "a", Provider: "codex", Status: "disabled"}, {ID: " ", Provider: "codex"}}); !handled || !errors.Is(err, store.ErrConcurrentLimit) {
		t.Fatalf("all unusable = %t %v", handled, err)
	}
}
