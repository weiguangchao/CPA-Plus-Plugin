package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type mockCPA struct {
	mu           sync.Mutex
	credentials  []credential
	quotas       map[authIndex][]byte
	failList     bool
	failQuota    bool
	lists        int
	queries      int
	quotaStarted chan struct{}
	releaseQuota <-chan struct{}
	startOnce    sync.Once
}

func (m *mockCPA) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-management-key" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	m.mu.Lock()
	status := http.StatusOK
	var raw []byte
	quotaRequest := false
	switch {
	case r.URL.Path == "/v8/management/credentials" && r.Method == http.MethodGet:
		m.lists++
		if m.failList {
			status = http.StatusServiceUnavailable
		} else {
			raw, _ = json.Marshal(map[string]any{"files": m.credentials})
		}
	case r.URL.Path == "/v8/management/requests/api-call" && r.Method == http.MethodPost:
		quotaRequest = true
		m.queries++
		var body struct {
			Index authIndex `json:"auth_index"`
		}
		if m.failQuota {
			status = http.StatusBadGateway
		} else if json.NewDecoder(r.Body).Decode(&body) != nil || m.quotas[body.Index] == nil {
			status = http.StatusBadRequest
		} else {
			raw, _ = json.Marshal(map[string]any{"status_code": 200, "body": string(m.quotas[body.Index])})
		}
	default:
		status = http.StatusNotFound
	}
	releaseQuota, quotaStarted := m.releaseQuota, m.quotaStarted
	m.mu.Unlock()
	if quotaRequest && releaseQuota != nil {
		m.startOnce.Do(func() { close(quotaStarted) })
		select {
		case <-releaseQuota:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func (m *mockCPA) counts() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lists, m.queries
}

type testClock struct{ ns atomic.Int64 }

func (c *testClock) now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *testClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

func newTestEngine(t *testing.T, mock *mockCPA, settings ...string) (*engine, *testClock) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(mock.serveHTTP))
	t.Cleanup(server.Close)
	cfg, err := parseConfig([]byte("cpa_url: " + server.URL + "\nmanagement_key: test-management-key\n" + strings.Join(settings, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(cfg)
	clock := &testClock{}
	clock.ns.Store(testNow.UnixNano())
	e.now = clock.now
	e.start()
	t.Cleanup(e.stop)
	return e, clock
}

func mockCredentials() []credential {
	return []credential{
		{ID: "a", Index: "index-a", Provider: "codex"},
		{ID: "b", Index: "index-b", Provider: "codex"},
	}
}

func pickRequest() pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{Provider: "codex", Model: "gpt-5.5", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}}}
}

func availableMock() *mockCPA {
	return &mockCPA{credentials: mockCredentials(), quotas: map[authIndex][]byte{
		"index-a": quotaJSON(.5, .5, testNow.Add(2*time.Hour).Format(time.RFC3339), nil),
		"index-b": quotaJSON(.5, .5, testNow.Add(time.Hour).Format(time.RFC3339), nil),
	}}
}

func assertPick(t *testing.T, e *engine, req pluginapi.SchedulerPickRequest, want string) {
	t.Helper()
	if got := e.pick(req); !got.Handled || got.AuthID != want || got.Reject {
		t.Fatalf("pick = %+v, want %s", got, want)
	}
}

func assertCounts(t *testing.T, mock *mockCPA, lists, quotas int) {
	t.Helper()
	if l, q := mock.counts(); l != lists || q != quotas {
		t.Fatalf("API calls: lists=%d quotas=%d, want %d/%d", l, q, lists, quotas)
	}
}

func awaitRefresh(t *testing.T, e *engine) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.mu.Lock()
		loading := false
		for _, state := range e.routes {
			loading = loading || state.loading != nil
		}
		e.mu.Unlock()
		if !loading {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDemandLoadingAndStickyTTL(t *testing.T) {
	mock := availableMock()
	e, clock := newTestEngine(t, mock)
	time.Sleep(20 * time.Millisecond)
	assertCounts(t, mock, 0, 0)
	assertPick(t, e, pickRequest(), "b")
	assertCounts(t, mock, 1, 2)
	mock.mu.Lock()
	mock.quotas["index-a"] = quotaJSON(.5, .5, testNow.Add(10*time.Minute).Format(time.RFC3339), nil)
	mock.mu.Unlock()
	clock.advance(4 * time.Minute)
	for i := 0; i < 20; i++ {
		if got := e.pick(pickRequest()); !got.Handled || got.AuthID != "b" {
			t.Fatal("valid selection was not sticky")
		}
	}
	assertCounts(t, mock, 1, 2)
	clock.advance(time.Minute)
	assertCounts(t, mock, 1, 2)
	assertPick(t, e, pickRequest(), "a")
	assertCounts(t, mock, 2, 4)
	other := newEngine(e.cfg)
	defer other.stop()
	if got := other.pick(pickRequest()); got.Handled {
		t.Fatal("new engine inherited selection")
	}
}

func TestFirstPickPrefersAccountWithBothWindows(t *testing.T) {
	mock := availableMock()
	mock.quotas["index-a"] = []byte(`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":10,"reset_after_seconds":3600}}}`)
	e, _ := newTestEngine(t, mock)
	assertPick(t, e, pickRequest(), "b")
	assertCounts(t, mock, 1, 2)
}

func TestPinnedRequestsPreserveOrdinarySelection(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		value    any
	}{
		{"other account", "a", "a"},
		{"cached account", "b", "b"},
		{"trimmed string", "a", " a \n"},
		{"bytes", "a", []byte("a")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := availableMock()
			mock.quotas["index-a"] = []byte(`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":10,"reset_after_seconds":3600}}}`)
			e, _ := newTestEngine(t, mock)
			assertPick(t, e, pickRequest(), "b")
			req := pickRequest()
			req.Options.Metadata = map[string]any{"pinned_auth_id": tc.value}
			req.Candidates = []pluginapi.SchedulerAuthCandidate{{ID: tc.id, Provider: "codex"}}
			if got := e.pick(req); got.Handled || got.Reject || got.AuthID != "" {
				t.Fatalf("pinned pick = %+v, want CPA fallback", got)
			}
			assertCounts(t, mock, 1, 2)
			assertPick(t, e, pickRequest(), "b")
			assertCounts(t, mock, 1, 2)
		})
	}
}

func TestPinnedColdRequestDoesNotDelayOrdinarySelection(t *testing.T) {
	mock := availableMock()
	e, _ := newTestEngine(t, mock)
	req := pickRequest()
	req.Options.Metadata = map[string]any{"pinned_auth_id": "a"}
	if got := e.pick(req); got.Handled || got.Reject || got.AuthID != "" {
		t.Fatalf("pinned cold pick = %+v, want CPA fallback", got)
	}
	assertCounts(t, mock, 0, 0)
	assertPick(t, e, pickRequest(), "b")
	assertCounts(t, mock, 1, 2)
}

func TestUnpinnedSingleCandidateStillUsesQuotaScheduling(t *testing.T) {
	for _, value := range []any{nil, "", " \t\n", []byte{}, []byte(" \n"), 1, true} {
		t.Run(fmt.Sprintf("%T/%v", value, value), func(t *testing.T) {
			mock := availableMock()
			e, _ := newTestEngine(t, mock)
			req := pickRequest()
			req.Options.Metadata = map[string]any{"pinned_auth_id": value}
			req.Candidates = req.Candidates[:1]
			assertPick(t, e, req, "a")
			assertCounts(t, mock, 1, 1)
		})
	}
}

func TestCandidateRemovalRefreshesOnlyRemainingAccounts(t *testing.T) {
	mock := availableMock()
	e, _ := newTestEngine(t, mock)
	assertPick(t, e, pickRequest(), "b")
	req := pickRequest()
	req.Candidates = req.Candidates[:1]
	assertPick(t, e, req, "a")
	assertCounts(t, mock, 2, 3)
	assertPick(t, e, pickRequest(), "a")
	assertCounts(t, mock, 2, 3)
}

func TestModelAndProviderRouteCachesAreIndependent(t *testing.T) {
	mock := availableMock()
	mock.quotas["index-b"] = []byte(`{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":50,"reset_at":"2026-10-29T13:00:00Z"},"secondary_window":{"limit_window_seconds":604800,"used_percent":50}},"additional_rate_limits":[{"limit_name":"gpt-5.3-codex-spark","rate_limit":{"secondary_window":{"limit_window_seconds":604800,"used_percent":100,"reset_at":"2026-10-29T12:00:05Z"}}}]}`)
	e, clock := newTestEngine(t, mock)
	assertPick(t, e, pickRequest(), "b")
	spark := pickRequest()
	spark.Model = "gpt-5.3-codex-spark"
	assertPick(t, e, spark, "a")
	clock.advance(5 * time.Second)
	assertPick(t, e, pickRequest(), "b")
	assertCounts(t, mock, 2, 4)
	mixed := pickRequest()
	mixed.Provider, mixed.Providers = "mixed", []string{"codex", "claude"}
	assertPick(t, e, mixed, "b")
	mixed.Providers = []string{"claude", "codex"}
	assertPick(t, e, mixed, "b")
	assertCounts(t, mock, 3, 6)
}

func TestKnownResetEndsStickinessOnNextRequest(t *testing.T) {
	for _, window := range []string{"five-hour", "weekly", "other account"} {
		t.Run(window, func(t *testing.T) {
			mock := availableMock()
			reset := testNow.Add(5 * time.Second).Format(time.RFC3339)
			switch window {
			case "five-hour":
				mock.quotas["index-b"] = quotaJSON(.5, .5, reset, nil)
			case "weekly":
				mock.quotas["index-b"] = quotaJSON(.5, .5, testNow.Add(time.Hour).Format(time.RFC3339), reset)
			case "other account":
				mock.quotas["index-a"] = quotaJSON(0, .5, reset, nil)
			}
			e, clock := newTestEngine(t, mock)
			assertPick(t, e, pickRequest(), "b")
			clock.advance(5 * time.Second)
			mock.mu.Lock()
			mock.quotas["index-a"] = quotaJSON(.5, .5, testNow.Add(time.Minute).Format(time.RFC3339), nil)
			mock.quotas["index-b"] = quotaJSON(.5, .5, testNow.Add(time.Hour).Format(time.RFC3339), nil)
			mock.mu.Unlock()
			assertCounts(t, mock, 1, 2)
			assertPick(t, e, pickRequest(), "a")
			assertCounts(t, mock, 2, 4)
		})
	}
}

func TestConcurrentMissesShareOneRefreshAndHitsStaySticky(t *testing.T) {
	release := make(chan struct{})
	mock := availableMock()
	mock.quotaStarted, mock.releaseQuota = make(chan struct{}), release
	e, _ := newTestEngine(t, mock)
	results := make(chan pluginapi.SchedulerPickResponse, 33)
	go func() { results <- e.pick(pickRequest()) }()
	select {
	case <-mock.quotaStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("quota query did not start")
	}
	pinned := pickRequest()
	pinned.Options.Metadata = map[string]any{"pinned_auth_id": "a"}
	pinned.Candidates = pinned.Candidates[:1]
	pinnedResult := make(chan pluginapi.SchedulerPickResponse, 1)
	go func() { pinnedResult <- e.pick(pinned) }()
	select {
	case got := <-pinnedResult:
		if got.Handled || got.Reject || got.AuthID != "" {
			t.Fatalf("pinned pick during refresh = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("pinned request waited for ordinary refresh")
	}
	assertCounts(t, mock, 1, 1)
	for i := 0; i < 32; i++ {
		go func() { results <- e.pick(pickRequest()) }()
	}
	select {
	case got := <-results:
		t.Fatalf("scheduler returned before quota query completed: %+v", got)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 33; i++ {
		select {
		case got := <-results:
			if !got.Handled || got.AuthID != "b" || got.Reject {
				t.Fatalf("first pick = %+v, want b", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("refresh did not wake all waiting requests")
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := e.pick(pickRequest()); !got.Handled || got.AuthID != "b" {
				t.Errorf("sticky hit = %+v", got)
			}
		}()
	}
	wg.Wait()
	assertCounts(t, mock, 1, 2)
}

func TestWaitTimeoutIncludesQueueAndDoesNotBlockCacheHits(t *testing.T) {
	mock := availableMock()
	e, _ := newTestEngine(t, mock)
	e.cfg.RequestTimeout = 150 * time.Millisecond
	assertPick(t, e, pickRequest(), "b")
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	mock.mu.Lock()
	mock.quotaStarted, mock.releaseQuota = make(chan struct{}), release
	started := mock.quotaStarted
	mock.mu.Unlock()
	slow := pickRequest()
	slow.Model = "slow-model"
	results := make(chan pluginapi.SchedulerPickResponse, 1)
	go func() { results <- e.pick(slow) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("quota query did not start")
	}
	hit := make(chan pluginapi.SchedulerPickResponse, 1)
	go func() { hit <- e.pick(pickRequest()) }()
	select {
	case got := <-hit:
		if !got.Handled || got.AuthID != "b" {
			t.Fatalf("cached route = %+v, want b", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cache hit blocked behind another route's query")
	}
	queued := pickRequest()
	queued.Model = "queued-model"
	before := time.Now()
	if got := e.pick(queued); got.Handled || got.Reject || got.AuthID != "" {
		t.Fatalf("queued timeout = %+v, want CPA fallback", got)
	}
	if elapsed := time.Since(before); elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Fatalf("queued wait = %s, want bounded wait near 150ms", elapsed)
	}
	select {
	case got := <-results:
		if got.Handled || got.Reject || got.AuthID != "" {
			t.Fatalf("query timeout = %+v, want CPA fallback", got)
		}
	case <-time.After(time.Second):
		t.Fatal("active query exceeded scheduler wait limit")
	}
	assertCounts(t, mock, 2, 3)
	releaseAll()
	assertPick(t, e, slow, "b")
	assertPick(t, e, queued, "b")
	assertCounts(t, mock, 3, 6)
}

func TestInFlightResultMustStillMatchCurrentCandidates(t *testing.T) {
	release := make(chan struct{})
	mock := availableMock()
	mock.quotaStarted, mock.releaseQuota = make(chan struct{}), release
	e, _ := newTestEngine(t, mock)
	fullResult := make(chan pluginapi.SchedulerPickResponse, 1)
	go func() { fullResult <- e.pick(pickRequest()) }()
	select {
	case <-mock.quotaStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("quota query did not start")
	}
	onlyA := pickRequest()
	onlyA.Candidates = onlyA.Candidates[:1]
	restrictedResult := make(chan pluginapi.SchedulerPickResponse, 1)
	go func() { restrictedResult <- e.pick(onlyA) }()
	close(release)
	select {
	case got := <-restrictedResult:
		if !got.Handled || got.AuthID != "a" || got.Reject {
			t.Fatalf("restricted pick = %+v, want a", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("candidate change did not finish reselection")
	}
	select {
	case got := <-fullResult:
		if !got.Handled || (got.AuthID != "a" && got.AuthID != "b") || got.Reject {
			t.Fatalf("full pick = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initial request did not finish")
	}
	assertCounts(t, mock, 2, 3)
}

func TestFailedSelectionThrottlesUntilRequestAfterRetryDeadline(t *testing.T) {
	for _, failure := range []string{"list", "quota", "exhausted", "no reset"} {
		t.Run(failure, func(t *testing.T) {
			mock := availableMock()
			e, clock := newTestEngine(t, mock)
			assertPick(t, e, pickRequest(), "b")
			mock.mu.Lock()
			switch failure {
			case "list":
				mock.failList = true
			case "quota":
				mock.failQuota = true
			case "exhausted":
				mock.quotas["index-a"], mock.quotas["index-b"] = quotaJSON(0, .5, nil, nil), quotaJSON(.5, 0, nil, nil)
			case "no reset":
				mock.quotas["index-a"], mock.quotas["index-b"] = quotaJSON(.5, .5, nil, nil), quotaJSON(.5, .5, nil, nil)
			}
			mock.mu.Unlock()
			clock.advance(e.cfg.CacheTTL)
			if got := e.pick(pickRequest()); got.Handled {
				t.Fatal("expired selection reused")
			}
			awaitRefresh(t, e)
			lists, quotas := mock.counts()
			for i := 0; i < 20; i++ {
				if got := e.pick(pickRequest()); got.Handled || got.Reject {
					t.Fatal("failed selection must delegate without quota filtering")
				}
			}
			assertCounts(t, mock, lists, quotas)
			mock.mu.Lock()
			mock.failList, mock.failQuota = false, false
			mock.quotas = availableMock().quotas
			mock.mu.Unlock()
			clock.advance(e.cfg.RetryInterval)
			assertCounts(t, mock, lists, quotas)
			assertPick(t, e, pickRequest(), "b")
			assertCounts(t, mock, lists+1, quotas+2)
		})
	}
}

func TestShortCacheTTLDoesNotBypassFailedQueryRetryDelay(t *testing.T) {
	for _, failure := range []string{"list", "no reset"} {
		t.Run(failure, func(t *testing.T) {
			mock := availableMock()
			initialQuotas := 0
			if failure == "list" {
				mock.failList = true
			} else {
				initialQuotas = 2
				mock.quotas["index-a"], mock.quotas["index-b"] = quotaJSON(.5, .5, nil, nil), quotaJSON(.5, .5, nil, nil)
			}
			e, clock := newTestEngine(t, mock, "cache_ttl: 1s")
			e.pick(pickRequest())
			awaitRefresh(t, e)
			assertCounts(t, mock, 1, initialQuotas)
			clock.advance(2 * time.Second)
			if got := e.pick(pickRequest()); got.Handled {
				t.Fatal("unsuccessful selection did not delegate")
			}
			awaitRefresh(t, e)
			assertCounts(t, mock, 1, initialQuotas)
			mock.mu.Lock()
			mock.failList = false
			mock.quotas = availableMock().quotas
			mock.mu.Unlock()
			clock.advance(28 * time.Second)
			assertPick(t, e, pickRequest(), "b")
			assertCounts(t, mock, 2, initialQuotas+2)
		})
	}
}

func TestKnownResetCanShortenUnsuccessfulSelectionRetry(t *testing.T) {
	mock := availableMock()
	mock.quotas["index-a"] = quotaJSON(0, .5, testNow.Add(5*time.Second).Format(time.RFC3339), nil)
	mock.quotas["index-b"] = quotaJSON(.5, 0, nil, nil)
	e, clock := newTestEngine(t, mock)
	e.pick(pickRequest())
	awaitRefresh(t, e)
	clock.advance(5 * time.Second)
	mock.mu.Lock()
	mock.quotas = availableMock().quotas
	mock.mu.Unlock()
	assertPick(t, e, pickRequest(), "b")
	assertCounts(t, mock, 2, 4)
}

func TestSlowRefreshCannotPublishPastItsCacheDeadline(t *testing.T) {
	release := make(chan struct{})
	mock := availableMock()
	mock.quotaStarted, mock.releaseQuota = make(chan struct{}), release
	e, clock := newTestEngine(t, mock)
	result := make(chan pluginapi.SchedulerPickResponse, 1)
	go func() { result <- e.pick(pickRequest()) }()
	select {
	case <-mock.quotaStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("quota query did not start")
	}
	clock.advance(e.cfg.CacheTTL)
	close(release)
	select {
	case got := <-result:
		if got.Handled || got.Reject {
			t.Fatalf("late first pick = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late response did not wake waiting request")
	}
	awaitRefresh(t, e)
	if got := e.pick(pickRequest()); got.Handled {
		t.Fatal("late response extended the cache deadline")
	}
	awaitRefresh(t, e)
	assertCounts(t, mock, 1, 1)
	clock.advance(e.cfg.RetryInterval)
	assertPick(t, e, pickRequest(), "b")
	assertCounts(t, mock, 2, 3)
}

func TestTieRotationOccursOnlyOnReselection(t *testing.T) {
	mock := availableMock()
	mock.quotas["index-a"] = mock.quotas["index-b"]
	e, clock := newTestEngine(t, mock)
	assertPick(t, e, pickRequest(), "a")
	for i := 0; i < 10; i++ {
		assertPick(t, e, pickRequest(), "a")
	}
	clock.advance(e.cfg.CacheTTL)
	assertPick(t, e, pickRequest(), "b")
	assertCounts(t, mock, 2, 4)
}

func TestNoEligibleHostCandidatesDoNotQueryAPI(t *testing.T) {
	mock := availableMock()
	e, _ := newTestEngine(t, mock)
	for _, candidates := range [][]pluginapi.SchedulerAuthCandidate{nil, {{ID: "x", Provider: "unsupported"}}} {
		req := pickRequest()
		req.Candidates = candidates
		if got := e.pick(req); got.Handled || got.Reject {
			t.Fatal("unsupported request must delegate")
		}
	}
	assertCounts(t, mock, 0, 0)
}

func TestStopCancelsManagementRequest(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	cfg, err := parseConfig([]byte("cpa_url: " + server.URL + "\nmanagement_key: test-management-key"))
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(cfg)
	e.start()
	defer e.stop()
	result := make(chan pluginapi.SchedulerPickResponse, 1)
	go func() { result <- e.pick(pickRequest()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("on-demand discovery did not start")
	}
	stopped := make(chan struct{})
	go func() { e.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel pending API request")
	}
	select {
	case got := <-result:
		if got.Handled || got.Reject {
			t.Fatalf("stopped pending pick = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not wake waiting scheduler")
	}
	if got := e.pick(pickRequest()); got.Handled {
		t.Fatal("stopped plugin handled a request")
	}
}

func TestManagementRejectsRedirectsAndMalformedLists(t *testing.T) {
	for _, body := range []string{`{}`, `{"files":null}`, `<html>login</html>`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			a := newManagementAPI(config{CPAURL: server.URL, RequestTimeout: time.Second})
			defer a.http.CloseIdleConnections()
			if _, err := a.credentials(context.Background()); err == nil {
				t.Fatal("malformed credential list accepted")
			}
		})
	}
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer server.Close()
	a := newManagementAPI(config{CPAURL: server.URL, RequestTimeout: time.Second, ManagementKey: "test-management-key"})
	defer a.http.CloseIdleConnections()
	if _, err := a.credentials(context.Background()); err == nil || redirected {
		t.Fatal("management redirect was followed")
	}
}

func TestPluginLifecycleAndDefaultFallback(t *testing.T) {
	defer shutdown()
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte("cpa_url: " + server.URL + "\nmanagement_key: test-management-key")})
	registrationRaw, err := handleMethod(pluginabi.MethodPluginRegister, raw)
	if err != nil {
		t.Fatal(err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(registrationRaw, &envelope); err != nil || !envelope.OK || !strings.Contains(string(envelope.Result), `"scheduler":true`) {
		t.Fatalf("invalid plugin registration: %s", registrationRaw)
	}
	request, _ := json.Marshal(pickRequest())
	for _, method := range []string{pluginabi.MethodSchedulerPick, pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginReconfigure, pluginabi.MethodPluginShutdown, pluginabi.MethodSchedulerPick} {
		input := request
		if method == pluginabi.MethodPluginReconfigure {
			input = raw
		}
		result, err := handleMethod(method, input)
		if err != nil {
			t.Fatal(err)
		}
		if method == pluginabi.MethodSchedulerPick {
			_ = json.Unmarshal(result, &envelope)
			var response pluginapi.SchedulerPickResponse
			_ = json.Unmarshal(envelope.Result, &response)
			if response.Handled || response.Reject || response.AuthID != "" {
				t.Fatalf("fallback did not preserve native CPA selection: %+v", response)
			}
		}
	}
}

func TestConfigValidation(t *testing.T) {
	const valid = "management_key: test-management-key\n"
	cfg, err := parseConfig([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ManagementKey != "test-management-key" {
		t.Fatal("direct management key was not loaded")
	}
	if _, err := parseConfig(nil); err != nil {
		t.Fatal("unconfigured plugin must remain available for form configuration")
	}
	for _, raw := range []string{
		"cpa_url: file:///tmp/cpa", "cpa_url: http://user:secret@localhost:8317",
		"cache_ttl: 0s", "retry_interval: 0s", "request_timeout: 0s",
	} {
		if _, err := parseConfig([]byte(valid + raw)); err == nil {
			t.Errorf("invalid configuration accepted: %s", raw)
		}
	}
	for _, raw := range []string{`management_key: "test-secret\nline"`, `management_key: "test-secret\rline"`} {
		if _, err := parseConfig([]byte(raw)); err == nil {
			t.Error("invalid management key accepted")
		} else if strings.Contains(err.Error(), "test-secret") {
			t.Fatal("management key included in validation error")
		}
	}
}

func TestEmptyManagementKeyDelegatesWithoutQueries(t *testing.T) {
	mock := availableMock()
	server := httptest.NewServer(http.HandlerFunc(mock.serveHTTP))
	defer server.Close()
	for _, value := range []string{`""`, `"  "`} {
		cfg, err := parseConfig([]byte("cpa_url: " + server.URL + "\nmanagement_key: " + value))
		if err != nil {
			t.Fatal(err)
		}
		e := newEngine(cfg)
		e.start()
		for i := 0; i < 10; i++ {
			if got := e.pick(pickRequest()); got.Handled || got.Reject {
				t.Fatal("unconfigured plugin did not delegate")
			}
		}
		e.stop()
		assertCounts(t, mock, 0, 0)
	}
}

func TestAllSupportedProvidersEnabledWithoutConfiguration(t *testing.T) {
	for _, provider := range []string{"claude", "anthropic", "codex"} {
		t.Run(provider, func(t *testing.T) {
			mock := availableMock()
			mock.credentials = []credential{{ID: "a", Index: "index-a", Provider: provider}}
			if provider != "codex" {
				mock.quotas["index-a"] = []byte(`{"five_hour":{"utilization":50,"resets_at":"2026-10-29T13:00:00Z"},"seven_day":{"utilization":20}}`)
			}
			e, _ := newTestEngine(t, mock, "providers: []")
			req := pluginapi.SchedulerPickRequest{Provider: provider, Model: "model", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: provider}}}
			assertPick(t, e, req, "a")
			assertCounts(t, mock, 1, 1)
		})
	}
}
