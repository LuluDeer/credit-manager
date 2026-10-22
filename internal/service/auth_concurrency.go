package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yuluo688/credit-manager/internal/store"
)

// AuthPickCandidate is one host auth offered to the plugin scheduler.
type AuthPickCandidate struct {
	ID       string
	Provider string
	// Priority is CPA's priority tier (higher wins). With
	// SchedulerAcrossPriorities the host sends every available tier.
	Priority int
	// Status is CPA's host-visible auth status; CPA already filters
	// cooldown/disabled auths, this is a defensive second check.
	Status string
}

// candidateUsable rejects candidates CPA marks as not routable.
func candidateUsable(c AuthPickCandidate) bool {
	switch strings.ToLower(strings.TrimSpace(c.Status)) {
	case "disabled":
		return false
	}
	return true
}

// highestTier returns the available candidates in the highest priority tier.
func highestTier(available []AuthPickCandidate) []AuthPickCandidate {
	if len(available) <= 1 {
		return available
	}
	best := available[0].Priority
	for _, c := range available[1:] {
		if c.Priority > best {
			best = c.Priority
		}
	}
	tier := make([]AuthPickCandidate, 0, len(available))
	for _, c := range available {
		if c.Priority == best {
			tier = append(tier, c)
		}
	}
	return tier
}

func (s *Service) SetAuthConcurrencyLimit(ctx context.Context, provider, authID string, maxConcurrent int64) (AuthQuotaOverviewItem, error) {
	provider, authID = authLimitProvider(provider), strings.TrimSpace(authID)
	if provider == "" || authID == "" {
		return AuthQuotaOverviewItem{}, fmt.Errorf("%w: provider and auth id are required", store.ErrInvalidArgument)
	}
	if maxConcurrent < 0 {
		return AuthQuotaOverviewItem{}, fmt.Errorf("%w: max concurrent requests must not be negative", store.ErrInvalidArgument)
	}
	if err := s.store.UpsertAuthConcurrencyLimit(ctx, provider, authID, maxConcurrent); err != nil {
		return AuthQuotaOverviewItem{}, err
	}
	source, files, err := s.authQuotaFiles(ctx)
	if err != nil {
		return s.withAuthConcurrency(ctx, AuthQuotaOverviewItem{AuthID: authID, Provider: provider, MaxConcurrentRequests: maxConcurrent}), nil
	}
	for _, file := range files {
		if !matchAuthQuotaFile(file, provider, authID, "") {
			continue
		}
		item, ok := s.loadAuthQuotaItem(ctx, source, "", file, false)
		if !ok {
			continue
		}
		return s.withAuthConcurrency(ctx, item), nil
	}
	item := AuthQuotaOverviewItem{AuthID: authID, Provider: provider, MaxConcurrentRequests: maxConcurrent}
	return s.withAuthConcurrency(ctx, item), nil
}

// AuthConcurrencyTarget identifies one auth row for a batch concurrency update.
type AuthConcurrencyTarget struct {
	Provider string `json:"provider"`
	AuthID   string `json:"auth_id"`
}

// AuthConcurrencyBatchResult reports how many auth rows a batch update changed.
type AuthConcurrencyBatchResult struct {
	Updated               int   `json:"updated"`
	MaxConcurrentRequests int64 `json:"max_concurrent_requests"`
}

func (s *Service) SetAuthConcurrencyLimits(ctx context.Context, filter AuthQuotaFilter, targets []AuthConcurrencyTarget, maxConcurrent int64) (AuthConcurrencyBatchResult, error) {
	if maxConcurrent < 0 {
		return AuthConcurrencyBatchResult{}, fmt.Errorf("%w: max concurrent requests must not be negative", store.ErrInvalidArgument)
	}
	limits, err := s.authConcurrencyBatchLimits(ctx, filter, targets, maxConcurrent)
	if err != nil {
		return AuthConcurrencyBatchResult{}, err
	}
	if err := s.store.UpsertAuthConcurrencyLimits(ctx, limits); err != nil {
		return AuthConcurrencyBatchResult{}, err
	}
	return AuthConcurrencyBatchResult{Updated: len(limits), MaxConcurrentRequests: maxConcurrent}, nil
}

func (s *Service) authConcurrencyBatchLimits(ctx context.Context, filter AuthQuotaFilter, targets []AuthConcurrencyTarget, maxConcurrent int64) ([]store.AuthConcurrencyLimit, error) {
	_, files, err := s.authQuotaFiles(ctx)
	if err != nil {
		return nil, err
	}
	filter = normalizeAuthQuotaFilter(filter)
	want := map[string]struct{}{}
	for _, target := range targets {
		provider, authID := authLimitProvider(target.Provider), strings.TrimSpace(target.AuthID)
		if provider == "" || authID == "" {
			continue
		}
		want[provider+"\x00"+authID] = struct{}{}
	}
	restrict := len(want) > 0
	out := make([]store.AuthConcurrencyLimit, 0, len(files))
	seen := map[string]struct{}{}
	for _, file := range files {
		provider := quotaProvider(file.Provider)
		authID := first(file.ID, file.Name, file.AuthIndex)
		if provider == "" || authID == "" {
			continue
		}
		if !authQuotaFileMatchesProvider(file, filter.Provider) || !authQuotaFileMatchesQuery(file, provider, filter.Q) {
			continue
		}
		if saved, savedErr := s.store.GetAuthQuotaSnapshot(ctx, provider, authID); savedErr == nil && isAuthQuotaAPIKeySentinel(saved) {
			continue
		}
		key := provider + "\x00" + authID
		if restrict {
			if _, ok := want[key]; !ok {
				continue
			}
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, store.AuthConcurrencyLimit{Provider: provider, AuthID: authID, MaxConcurrentRequests: maxConcurrent})
	}
	return out, nil
}

func (s *Service) AdmitAuth(ctx context.Context, reservationID string, auth store.AuthIdentity) error {
	if s == nil || strings.TrimSpace(reservationID) == "" || auth.Empty() {
		return nil
	}
	provider, authID := authLimitIdentity(auth)
	if provider == "" || authID == "" {
		s.bindAuthCapture(reservationID, auth)
		return nil
	}
	limit, err := s.store.GetAuthConcurrencyLimit(ctx, provider, authID)
	if err != nil {
		return err
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.ensureAuthPendingLocked()
	if limit > 0 && s.activeAuthRequestsLocked(provider, authID, reservationID) >= limit {
		return store.ErrConcurrentLimit
	}
	s.bindAuthCaptureLocked(reservationID, auth)
	return nil
}

func (s *Service) PickAuth(ctx context.Context, candidates []AuthPickCandidate) (authID string, handled bool, err error) {
	return s.PickAuthForModel(ctx, "", candidates)
}

// PickAuthForModel takes over CPA's auth selection: it drops full / warming
// candidates, picks with the configured plugin routing strategy and attributes
// the request to the chosen auth at once so in-flight counts are live.
// Candidates arrive from CPA already filtered to the highest available
// priority tier; like CPA's selectors they are ordered by ID.
func (s *Service) PickAuthForModel(ctx context.Context, model string, candidates []AuthPickCandidate) (authID string, handled bool, err error) {
	return s.PickAuthForSession(ctx, model, "", candidates)
}

type authAffinityBinding struct {
	authID   string
	provider string
	expires  time.Time
}

// PickAuthForSession is PickAuthForModel plus optional session affinity: when
// enabled and sessionKey is set, a session stays on its bound auth while that
// auth is still an available (non-full) candidate; otherwise the base strategy
// picks and the session is rebound. Bindings expire after the idle TTL.
func (s *Service) PickAuthForSession(ctx context.Context, model, sessionKey string, candidates []AuthPickCandidate) (authID string, handled bool, err error) {
	return s.PickAuthForRequest(ctx, model, sessionKey, "", candidates)
}

// PickAuthForRequest is PickAuthForSession with exact request pairing: when
// reservationID names a live, still-unattributed reservation the pick is bound
// to it; otherwise the oldest unattributed reservation is used (legacy path).
func (s *Service) PickAuthForRequest(ctx context.Context, model, sessionKey, reservationID string, candidates []AuthPickCandidate) (authID string, handled bool, err error) {
	if s == nil || len(candidates) == 0 {
		return "", false, nil
	}
	limits, err := s.store.ListAuthConcurrencyLimits(ctx)
	if err != nil {
		return "", false, err
	}
	strategy := store.AuthRoutingRoundRobin
	affinity := false
	ttl := time.Duration(store.DefaultSessionAffinityTTLSeconds) * time.Second
	if settings, errSettings := s.store.GetAuthRoutingSettings(ctx); errSettings == nil {
		strategy = settings.Strategy
		affinity = settings.SessionAffinity
		if settings.SessionAffinityTTLSeconds > 0 {
			ttl = time.Duration(settings.SessionAffinityTTLSeconds) * time.Second
		}
	}
	sessionKey = strings.TrimSpace(sessionKey)
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.ensureAuthPendingLocked()
	available := make([]AuthPickCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		limit := authConcurrencyLimitOf(limits, candidate)
		provider, id := authLimitProvider(candidate.Provider), strings.TrimSpace(candidate.ID)
		if id == "" || !candidateUsable(candidate) {
			continue
		}
		if s.authWarmupBusyLocked(provider, id) {
			continue
		}
		if limit > 0 && provider != "" && s.activeAuthRequestsLocked(provider, id, "") >= limit {
			continue
		}
		available = append(available, candidate)
	}
	if len(available) == 0 {
		return "", true, store.ErrConcurrentLimit
	}
	sort.SliceStable(available, func(i, j int) bool { return strings.TrimSpace(available[i].ID) < strings.TrimSpace(available[j].ID) })
	now := time.Now()
	// Session affinity mirrors CPA's SessionAffinitySelector: the key is
	// provider::session::canonical-model (thinking suffix stripped). A
	// session key may carry "primary,fallback" (message-hash fallback: the
	// full system+user+assistant hash and the short system+user hash); the
	// primary binding wins, then the fallback, and both are (re)bound.
	affinityKey, fallbackAffinityKey := "", ""
	if affinity && sessionKey != "" {
		primary, fallback, _ := strings.Cut(sessionKey, ",")
		primary, fallback = strings.TrimSpace(primary), strings.TrimSpace(fallback)
		modelKey := canonicalModelKey(model)
		provider := authLimitProvider(available[0].Provider)
		if primary == "" {
			primary, fallback = fallback, ""
		}
		if primary != "" {
			affinityKey = provider + "::" + primary + "::" + modelKey
		}
		if fallback != "" && fallback != primary {
			fallbackAffinityKey = provider + "::" + fallback + "::" + modelKey
		}
		for _, key := range []string{affinityKey, fallbackAffinityKey} {
			if key == "" {
				continue
			}
			bound, ok := s.authAffinity[key]
			if !ok || !now.Before(bound.expires) {
				continue
			}
			for _, candidate := range available {
				if strings.TrimSpace(candidate.ID) == bound.authID {
					s.bindAffinityLocked(affinityKey, candidate, now.Add(ttl))
					if fallbackAffinityKey != "" {
						s.bindAffinityLocked(fallbackAffinityKey, candidate, now.Add(ttl))
					}
					s.bindPickLocked(reservationID, store.AuthIdentity{AuthID: bound.authID, Provider: candidate.Provider})
					return bound.authID, true, nil
				}
			}
		}
	}
	// Tiered fallback: only the highest priority tier that still has a
	// non-full, non-warming account is eligible; lower tiers are used only
	// when every higher-tier account is full or unavailable.
	available = highestTier(available)
	var chosen AuthPickCandidate
	switch strategy {
	case store.AuthRoutingFillFirst:
		chosen = available[0]
	case store.AuthRoutingLeastBusy:
		chosen = available[0]
		best := s.activeAuthRequestsLocked(chosen.Provider, strings.TrimSpace(chosen.ID), "")
		for _, candidate := range available[1:] {
			if n := s.activeAuthRequestsLocked(candidate.Provider, strings.TrimSpace(candidate.ID), ""); n < best {
				chosen, best = candidate, n
			}
		}
	default:
		chosen = s.nextAuthPickLocked(model, available)
	}
	if affinityKey != "" {
		s.bindAffinityLocked(affinityKey, chosen, now.Add(ttl))
	}
	if fallbackAffinityKey != "" {
		s.bindAffinityLocked(fallbackAffinityKey, chosen, now.Add(ttl))
	}
	s.bindPickLocked(reservationID, store.AuthIdentity{AuthID: strings.TrimSpace(chosen.ID), Provider: chosen.Provider})
	return strings.TrimSpace(chosen.ID), true, nil
}

func (s *Service) withAuthConcurrency(ctx context.Context, item AuthQuotaOverviewItem) AuthQuotaOverviewItem {
	items := []AuthQuotaOverviewItem{item}
	s.attachAuthConcurrency(ctx, items)
	return items[0]
}

func (s *Service) attachAuthConcurrency(ctx context.Context, items []AuthQuotaOverviewItem) {
	if s == nil || len(items) == 0 {
		return
	}
	limits, err := s.store.ListAuthConcurrencyLimits(ctx)
	if err != nil {
		limits = map[string]int64{}
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.ensureAuthPendingLocked()
	for i := range items {
		provider, authID := authLimitProvider(items[i].Provider), strings.TrimSpace(items[i].AuthID)
		if provider != "" && authID != "" {
			items[i].MaxConcurrentRequests = limits[provider+"\x00"+authID]
		}
		items[i].ActiveRequests = s.activeAuthRequestsLocked(provider, authID, "")
	}
}

func (s *Service) bindAuthCapture(reservationID string, auth store.AuthIdentity) {
	if s == nil {
		return
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.bindAuthCaptureLocked(reservationID, auth)
}

func (s *Service) bindAuthCaptureLocked(reservationID string, auth store.AuthIdentity) {
	s.ensureAuthPendingLocked()
	reservationID = strings.TrimSpace(reservationID)
	if reservationID == "" || auth.Empty() {
		return
	}
	pending := s.authPending[reservationID]
	if pending == nil {
		return
	}
	pending.auth = auth
	pending.hasAuth = true
}

// bindPickLocked attributes a scheduler pick to reservationID when it is a
// live unattributed reservation, else falls back to the oldest one.
func (s *Service) bindPickLocked(reservationID string, auth store.AuthIdentity) {
	if auth.Empty() {
		return
	}
	reservationID = strings.TrimSpace(reservationID)
	if reservationID != "" {
		// Exact pairing. A repeat pick for the same reservation is CPA
		// retrying on another auth (e.g. after a 429): move the in-flight
		// slot and usage attribution to the new auth instead of leaving it
		// on the failed one or leaking the pick onto an unrelated request.
		if pending := s.authPending[reservationID]; pending != nil && pending.active {
			pending.auth = auth
			pending.hasAuth = true
		}
		return
	}
	s.bindOldestUnattributedLocked(auth)
}

func (s *Service) bindOldestUnattributedLocked(auth store.AuthIdentity) {
	if auth.Empty() {
		return
	}
	var oldest *pendingAuthCapture
	for _, pending := range s.authPending {
		if pending == nil || !pending.active || pending.hasAuth {
			continue
		}
		if oldest == nil || pending.startedAt.Before(oldest.startedAt) {
			oldest = pending
		}
	}
	if oldest == nil {
		return
	}
	oldest.auth = auth
	oldest.hasAuth = true
}

func (s *Service) bindAffinityLocked(key string, candidate AuthPickCandidate, expires time.Time) {
	if s.authAffinity == nil {
		s.authAffinity = map[string]authAffinityBinding{}
	}
	if _, exists := s.authAffinity[key]; !exists && len(s.authAffinity) >= 65536 {
		now := time.Now()
		for k, b := range s.authAffinity {
			if now.After(b.expires) {
				delete(s.authAffinity, k)
			}
		}
		if len(s.authAffinity) >= 65536 {
			s.authAffinity = map[string]authAffinityBinding{}
		}
	}
	s.authAffinity[key] = authAffinityBinding{authID: strings.TrimSpace(candidate.ID), provider: candidate.Provider, expires: expires}
}

// nextAuthPickLocked mirrors CPA's RoundRobinSelector: one cursor per
// provider:model key, index = cursor % len(available).
// nextAuthPickLocked mirrors CPA's RoundRobinSelector: per
// provider:canonical-model key it remembers the last picked auth ID and
// resumes at the first candidate (sorted by ID) after it, wrapping to the
// head. Rotation therefore survives candidates dropping out and returning.
func (s *Service) nextAuthPickLocked(model string, available []AuthPickCandidate) AuthPickCandidate {
	key := authLimitProvider(available[0].Provider) + ":" + canonicalModelKey(model)
	if s.authPickLast == nil {
		s.authPickLast = map[string]string{}
	}
	if _, ok := s.authPickLast[key]; !ok && len(s.authPickLast) >= 4096 {
		s.authPickLast = map[string]string{}
	}
	index := 0
	if last := s.authPickLast[key]; last != "" {
		index = sort.Search(len(available), func(i int) bool { return strings.TrimSpace(available[i].ID) > last })
		if index >= len(available) {
			index = 0
		}
	}
	picked := available[index]
	s.authPickLast[key] = strings.TrimSpace(picked.ID)
	return picked
}

// canonicalModelKey mirrors CPA's canonicalModelKey: it strips a trailing
// thinking suffix "model(value)" (thinking.ParseSuffix).
func canonicalModelKey(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	open := strings.LastIndex(model, "(")
	if open == -1 || !strings.HasSuffix(model, ")") {
		return model
	}
	if name := strings.TrimSpace(model[:open]); name != "" {
		return name
	}
	return model
}

func (s *Service) activeAuthRequestsLocked(provider, authID, exceptReservation string) int64 {
	if authID == "" {
		return 0
	}
	provider = authLimitProvider(provider)
	exceptReservation = strings.TrimSpace(exceptReservation)
	var n int64
	for id, pending := range s.authPending {
		if pending == nil || !pending.active || !pending.hasAuth || id == exceptReservation {
			continue
		}
		pendingProvider, pendingID := authLimitIdentity(pending.auth)
		if pendingID == authID && (provider == "" || pendingProvider == provider) {
			n++
		}
	}
	for _, auth := range s.warmupHolds {
		holdProvider, holdID := authLimitIdentity(auth)
		if holdID == authID && (provider == "" || holdProvider == provider) {
			n++
		}
	}
	return n
}

func (s *Service) authWarmupBusyLocked(provider, authID string) bool {
	provider, authID = authLimitProvider(provider), strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	for _, auth := range s.warmupHolds {
		holdProvider, holdID := authLimitIdentity(auth)
		if holdID == authID && (provider == "" || holdProvider == provider) {
			return true
		}
	}
	return false
}

func authConcurrencyLimitOf(limits map[string]int64, candidate AuthPickCandidate) int64 {
	provider, authID := authLimitProvider(candidate.Provider), strings.TrimSpace(candidate.ID)
	if provider == "" || authID == "" || len(limits) == 0 {
		return 0
	}
	return limits[provider+"\x00"+authID]
}

func authLimitIdentity(auth store.AuthIdentity) (provider, authID string) {
	return authLimitProvider(auth.Provider), first(auth.AuthID, auth.AuthIndex)
}

func authLimitProvider(provider string) string {
	if normalized := quotaProvider(provider); normalized != "" {
		return normalized
	}
	return strings.ToLower(strings.TrimSpace(provider))
}

// AuthRoutingSettings returns the plugin auth pick strategy.
func (s *Service) AuthRoutingSettings(ctx context.Context) (store.AuthRoutingSettings, error) {
	return s.store.GetAuthRoutingSettings(ctx)
}

// UpdateAuthRoutingSettings persists the plugin auth pick strategy.
func (s *Service) UpdateAuthRoutingSettings(ctx context.Context, settings store.AuthRoutingSettings) (store.AuthRoutingSettings, error) {
	return s.store.UpsertAuthRoutingSettings(ctx, settings)
}
