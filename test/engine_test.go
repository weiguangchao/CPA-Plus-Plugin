package main

import (
	"context"
	"encoding/json"
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
	m.mu.Unlock()
	if quotaRequest && m.releaseQuota != nil {
		m.startOnce.Do(func() { close(m.quotaStarted) })
		select {
		case <-m.releaseQuota:
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

func awaitPick(t *testing.T, e *engine, req pluginapi.SchedulerPickRequest, want string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		got := e.pick(req)
		if got.Handled {
			if got.AuthID != want || got.Reject {
				t.Fatalf("pick = %+v, want %s", got, want)
			}
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", want)
		case <-tick.C:
		}
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
			loading = loading || state.loading
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
	if got := e.pick(pickRequest()); got.Handled || got.Reject {
		t.Fatal("cold cache must delegate")
	}
	awaitPick(t, e, pickRequest(), "b")
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
	if got := e.pick(pickRequest()); got.Handled {
		t.Fatal("expired selection must delegate during refresh")
	}
	awaitPick(t, e, pickRequest(), "a")
	assertCounts(t, mock, 2, 4)
	other := newEngine(e.cfg)
	defer other.stop()
	if got := other.pick(pickRequest()); got.Handled {
		t.Fatal("new engine inherited selection")
	}
}

func TestCandidateRemovalRefreshesOnlyRemainingAccounts(t *testing.T) {
	mock := availableMock()
	e, _ := newTestEngine(t, mock)
	awaitPick(t, e, pickRequest(), "b")
	req := pickRequest()
	req.Candidates = req.Candidates[:1]
	if got := e.pick(req); got.Handled {
		t.Fatal("removed account or old quota reused")
	}
	awaitPick(t, e, req, "a")
	assertCounts(t, mock, 2, 3)
	awaitPick(t, e, pickRequest(), "a")
	assertCounts(t, mock, 2, 3)
}

func TestModelAndProviderRouteCachesAreIndependent(t *testing.T) {
	mock := availableMock()
	mock.quotas["index-b"] = []byte(`{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":50,"reset_at":"2026-10-29T13:00:00Z"},"secondary_window":{"limit_window_seconds":604800,"used_percent":50}},"additional_rate_limits":[{"limit_name":"gpt-5.3-codex-spark","rate_limit":{"secondary_window":{"limit_window_seconds":604800,"used_percent":100,"reset_at":"2026-10-29T12:00:05Z"}}}]}`)
	e, clock := newTestEngine(t, mock)
	awaitPick(t, e, pickRequest(), "b")
	spark := pickRequest()
	spark.Model = "gpt-5.3-codex-spark"
	awaitPick(t, e, spark, "a")
	clock.advance(5 * time.Second)
	awaitPick(t, e, pickRequest(), "b")
	assertCounts(t, mock, 2, 4)
	mixed := pickRequest()
	mixed.Provider, mixed.Providers = "mixed", []string{"codex", "claude"}
	awaitPick(t, e, mixed, "b")
	mixed.Providers = []string{"claude", "codex"}
	awaitPick(t, e, mixed, "b")
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
			awaitPick(t, e, pickRequest(), "b")
			clock.advance(5 * time.Second)
			mock.mu.Lock()
			mock.quotas["index-a"] = quotaJSON(.5, .5, testNow.Add(time.Minute).Format(time.RFC3339), nil)
			mock.quotas["index-b"] = quotaJSON(.5, .5, testNow.Add(time.Hour).Format(time.RFC3339), nil)
			mock.mu.Unlock()
			assertCounts(t, mock, 1, 2)
			if got := e.pick(pickRequest()); got.Handled {
				t.Fatal("reset account reused without a new observation")
			}
			awaitPick(t, e, pickRequest(), "a")
			assertCounts(t, mock, 2, 4)
		})
	}
}

func TestConcurrentMissesShareOneRefreshAndHitsStaySticky(t *testing.T) {
	release := make(chan struct{})
	mock := availableMock()
	mock.quotaStarted, mock.releaseQuota = make(chan struct{}), release
	e, _ := newTestEngine(t, mock)
	e.pick(pickRequest())
	select {
	case <-mock.quotaStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("quota query did not start")
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if got := e.pick(pickRequest()); got.Handled || got.Reject {
					t.Errorf("pending refresh must delegate: %+v", got)
					return
				}
			}
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler waited for network")
	}
	close(release)
	awaitPick(t, e, pickRequest(), "b")
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

func TestInFlightResultMustStillMatchCurrentCandidates(t *testing.T) {
	release := make(chan struct{})
	mock := availableMock()
	mock.quotaStarted, mock.releaseQuota = make(chan struct{}), release
	e, _ := newTestEngine(t, mock)
	e.pick(pickRequest())
	select {
	case <-mock.quotaStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("quota query did not start")
	}
	onlyA := pickRequest()
	onlyA.Candidates = onlyA.Candidates[:1]
	if got := e.pick(onlyA); got.Handled {
		t.Fatal("in-flight selection escaped candidate check")
	}
	close(release)
	awaitPick(t, e, onlyA, "a")
	assertCounts(t, mock, 2, 3)
}

func TestFailedSelectionThrottlesUntilRequestAfterRetryDeadline(t *testing.T) {
	for _, failure := range []string{"list", "quota", "exhausted", "no reset"} {
		t.Run(failure, func(t *testing.T) {
			mock := availableMock()
			e, clock := newTestEngine(t, mock)
			awaitPick(t, e, pickRequest(), "b")
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
			awaitPick(t, e, pickRequest(), "b")
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
			awaitPick(t, e, pickRequest(), "b")
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
	awaitPick(t, e, pickRequest(), "b")
	assertCounts(t, mock, 2, 4)
}

func TestSlowRefreshCannotPublishPastItsCacheDeadline(t *testing.T) {
	release := make(chan struct{})
	mock := availableMock()
	mock.quotaStarted, mock.releaseQuota = make(chan struct{}), release
	e, clock := newTestEngine(t, mock)
	e.pick(pickRequest())
	select {
	case <-mock.quotaStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("quota query did not start")
	}
	clock.advance(e.cfg.CacheTTL)
	close(release)
	awaitRefresh(t, e)
	if got := e.pick(pickRequest()); got.Handled {
		t.Fatal("late response extended the cache deadline")
	}
	awaitRefresh(t, e)
	assertCounts(t, mock, 1, 1)
	clock.advance(e.cfg.RetryInterval)
	awaitPick(t, e, pickRequest(), "b")
	assertCounts(t, mock, 2, 3)
}

func TestTieRotationOccursOnlyOnReselection(t *testing.T) {
	mock := availableMock()
	mock.quotas["index-a"] = mock.quotas["index-b"]
	e, clock := newTestEngine(t, mock)
	awaitPick(t, e, pickRequest(), "a")
	for i := 0; i < 10; i++ {
		awaitPick(t, e, pickRequest(), "a")
	}
	clock.advance(e.cfg.CacheTTL)
	awaitPick(t, e, pickRequest(), "b")
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
	e.pick(pickRequest())
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
		"cache_ttl: 0s", "retry_interval: 0s", "request_timeout: 0s", "providers: []", "providers: [unsupported]",
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
