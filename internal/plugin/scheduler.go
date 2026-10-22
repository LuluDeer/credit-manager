package plugin

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func pickAuth(raw []byte) ([]byte, error) {
	svc := service.Current()
	if svc == nil {
		return okEnvelope(pluginapi.SchedulerPickResponse{})
	}
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	candidates := make([]service.AuthPickCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		id := firstNonEmpty(candidate.ID)
		if id == "" {
			continue
		}
		candidates = append(candidates, service.AuthPickCandidate{ID: id, Provider: candidate.Provider, Priority: candidate.Priority, Status: candidate.Status})
	}
	if authID, handled, err := warmupTarget(req.Options.Headers, candidates); handled {
		if err != nil {
			return errorEnvelope("warmup_rejected", err.Error()), nil
		}
		return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: authID, Handled: true})
	}
	authID, handled, err := svc.PickAuthForRequest(context.Background(), req.Model, headerValueFold(req.Options.Headers, sessionAffinityHeader), headerValueFold(req.Options.Headers, reservationHeader), candidates)
	if err != nil {
		if errors.Is(err, store.ErrConcurrentLimit) {
			// Every candidate in every tier is full: terminate selection
			// cleanly instead of failing the RPC.
			return okEnvelope(pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: "concurrency_limit_reached", RejectReason: err.Error()})
		}
		return errorEnvelope("limit_rejected", err.Error()), nil
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  authID,
		Handled: handled,
	})
}
