package main

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type accountKey struct {
	id       authID
	provider string
}

type routeKey struct {
	provider, providers, model string
}

type selection struct {
	account    accountKey
	loadedAt   time.Time
	validUntil time.Time
	nextReset  time.Time
}

type routeCache struct {
	selection selection
	lastUsed  time.Time
	retryAt   time.Time
	loading   bool
	queued    *pluginapi.SchedulerPickRequest
}

type engine struct {
	cfg config
	api *managementAPI
	now func() time.Time

	mu        sync.Mutex
	routes    map[routeKey]*routeCache
	nextPrune time.Time
	stopped   bool
	wake      chan struct{}
	cancel    context.CancelFunc
	done      chan struct{}

	tie uint64
}

func newEngine(cfg config) *engine {
	return &engine{cfg: cfg, api: newManagementAPI(cfg), now: time.Now,
		routes: make(map[routeKey]*routeCache), wake: make(chan struct{}, 1)}
}

func (e *engine) start() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped || e.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel, e.done = cancel, make(chan struct{})
	go func() {
		defer close(e.done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-e.wake:
			}
			for ctx.Err() == nil {
				e.mu.Lock()
				var entry *routeCache
				var req pluginapi.SchedulerPickRequest
				for _, candidate := range e.routes {
					if candidate.queued != nil {
						entry, req = candidate, *candidate.queued
						entry.queued = nil
						break
					}
				}
				e.mu.Unlock()
				if entry == nil {
					break
				}
				result := e.loadSelection(ctx, req)
				e.mu.Lock()
				entry.selection, entry.loading = result, false
				entry.retryAt = e.now().Add(e.cfg.RetryInterval)
				if result.nextReset.After(e.now()) && result.nextReset.Before(entry.retryAt) {
					entry.retryAt = result.nextReset
				}
				e.mu.Unlock()
			}
		}
	}()
}

func (e *engine) stop() {
	e.mu.Lock()
	e.stopped = true
	cancel, done := e.cancel, e.done
	e.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	e.api.http.CloseIdleConnections()
}

func (e *engine) pick(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	if e.cfg.ManagementKey == "" {
		return pluginapi.SchedulerPickResponse{}
	}
	providers := append([]string(nil), req.Providers...)
	sort.Strings(providers)
	encodedProviders, _ := json.Marshal(providers)
	key := routeKey{provider: req.Provider, providers: string(encodedProviders), model: req.Model}

	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if candidate.ID != "" && e.cfg.manages(candidate.Provider) {
			candidates = append(candidates, pluginapi.SchedulerAuthCandidate{ID: candidate.ID, Provider: candidate.Provider})
		}
	}
	if len(candidates) == 0 {
		return pluginapi.SchedulerPickResponse{}
	}
	req = pluginapi.SchedulerPickRequest{Model: req.Model, Candidates: candidates}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return pluginapi.SchedulerPickResponse{}
	}
	now := e.now()
	if !now.Before(e.nextPrune) {
		for route, entry := range e.routes {
			if !entry.loading && !now.Before(entry.retryAt) && now.Sub(entry.lastUsed) >= e.cfg.CacheTTL {
				delete(e.routes, route)
			}
		}
		e.nextPrune = now.Add(e.cfg.CacheTTL)
	}
	entry := e.routes[key]
	if entry == nil {
		entry = &routeCache{}
		e.routes[key] = entry
	}
	entry.lastUsed = now
	if entry.loading {
		return pluginapi.SchedulerPickResponse{}
	}
	previous := entry.selection
	if previous.account.id != "" && !now.Before(previous.loadedAt) && now.Before(previous.validUntil) {
		for _, candidate := range candidates {
			if previous.account == (accountKey{authID(candidate.ID), candidate.Provider}) {
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: candidate.ID}
			}
		}
	}
	if previous.account.id == "" && now.Before(entry.retryAt) {
		return pluginapi.SchedulerPickResponse{}
	}
	entry.loading, entry.queued = true, &req
	select {
	case e.wake <- struct{}{}:
	default:
	}
	return pluginapi.SchedulerPickResponse{}
}

func (e *engine) loadSelection(ctx context.Context, req pluginapi.SchedulerPickRequest) selection {
	started := e.now()
	credentials, err := e.api.credentials(ctx)
	if err != nil {
		return selection{}
	}
	requested := make(map[accountKey]bool, len(req.Candidates))
	for _, c := range req.Candidates {
		requested[accountKey{authID(c.ID), c.Provider}] = true
	}
	quotas := make(map[authID]quotaSnapshot, len(requested))
	result := selection{loadedAt: started, validUntil: started.Add(e.cfg.CacheTTL)}
	for _, c := range credentials {
		if ctx.Err() != nil || !e.now().Before(result.validUntil) {
			return selection{}
		}
		if !requested[accountKey{c.ID, c.Provider}] || c.Disabled || c.Index == "" {
			continue
		}
		queriedAt := e.now()
		q, err := e.api.quota(ctx, c, queriedAt)
		if err != nil {
			continue
		}
		quotas[c.ID] = q
		if reset := q.nextReset(req.Model, started); !reset.IsZero() {
			if result.nextReset.IsZero() || reset.Before(result.nextReset) {
				result.nextReset = reset
			}
			if reset.Before(result.validUntil) {
				result.validUntil = reset
			}
		}
	}
	if !e.now().Before(result.validUntil) {
		return selection{}
	}
	winners := rankedAccounts(req, quotas, e.now(), e.cfg.CacheTTL)
	if len(winners) > 0 {
		id := winners[0]
		if len(winners) > 1 {
			id = winners[e.tie%uint64(len(winners))]
			e.tie++
		}
		for _, candidate := range req.Candidates {
			if candidate.ID == id {
				result.account = accountKey{authID(id), candidate.Provider}
				break
			}
		}
	}
	return result
}
