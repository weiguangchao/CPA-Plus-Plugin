package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var testNow = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func quotaJSON(five, week float64, fiveReset, weekReset any) []byte {
	raw, _ := json.Marshal(map[string]any{"rate_limit": map[string]any{
		"primary_window":   map[string]any{"limit_window_seconds": 18000, "used_percent": (1 - five) * 100, "reset_at": fiveReset},
		"secondary_window": map[string]any{"limit_window_seconds": 604800, "used_percent": (1 - week) * 100, "reset_at": weekReset},
	}})
	return raw
}

func parsedQuota(t *testing.T, raw []byte) quotaSnapshot {
	t.Helper()
	q, err := parseQuota(raw, "codex", testNow)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestSchedulingOrder(t *testing.T) {
	soon := testNow.Add(time.Hour).Format(time.RFC3339)
	later := testNow.Add(2 * time.Hour).Format(time.RFC3339)
	week := testNow.Add(4 * 24 * time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name string
		a, b []byte
		want []string
	}{
		{"nearest five-hour reset", quotaJSON(.5, .5, later, week), quotaJSON(.5, .5, soon, week), []string{"b"}},
		{"five-hour precedes earlier weekly reset", quotaJSON(.5, .5, later, week), quotaJSON(.5, .5, nil, soon), []string{"a"}},
		{"weekly fallback", quotaJSON(.5, .5, nil, later), quotaJSON(.5, .5, nil, soon), []string{"b"}},
		{"no reset delegates", quotaJSON(.5, .5, nil, nil), quotaJSON(.5, .5, nil, nil), nil},
		{"exhausted weekly excluded", quotaJSON(.5, 0, soon, week), quotaJSON(.5, .5, later, week), []string{"b"}},
		{"exhausted five-hour excluded from weekly fallback", quotaJSON(0, .5, nil, soon), quotaJSON(.5, .5, nil, later), []string{"b"}},
		{"both exhausted delegates", quotaJSON(0, .5, soon, week), quotaJSON(.5, 0, later, week), nil},
		{"past reset needs confirmation", quotaJSON(.5, .5, testNow.Add(-time.Second).Format(time.RFC3339), week), quotaJSON(.5, .5, later, week), []string{"b"}},
		{"ties are independent of host candidate order", quotaJSON(.5, .5, soon, week), quotaJSON(.5, .5, soon, week), []string{"a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quotas := map[authID]quotaSnapshot{"a": parsedQuota(t, tc.a), "b": parsedQuota(t, tc.b)}
			req := pluginapi.SchedulerPickRequest{Model: "gpt-5.5", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "b"}, {ID: "a"}}}
			if got := rankedAccounts(req, quotas, testNow, time.Minute); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("selected %v, want %v", got, tc.want)
			}
		})
	}
}

func TestQuotaExpiryAndHostCandidates(t *testing.T) {
	q := parsedQuota(t, quotaJSON(.5, .5, testNow.Add(time.Hour).Format(time.RFC3339), nil))
	req := pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a"}}}
	quotas := map[authID]quotaSnapshot{"a": q, "removed": q}
	if got := rankedAccounts(req, quotas, testNow.Add(time.Minute), time.Minute); len(got) != 0 {
		t.Fatalf("stale quota selected %v", got)
	}
	delete(quotas, "a")
	if got := rankedAccounts(req, quotas, testNow, time.Minute); len(got) != 0 {
		t.Fatalf("selected account outside host candidates: %v", got)
	}
}

func TestQuotaAdapters(t *testing.T) {
	for _, tc := range []struct {
		name, provider, model, body string
		usable                      bool
	}{
		{"Claude model limit blocks", "claude", "claude-sonnet-4", `{"five_hour":{"utilization":50},"seven_day":{"utilization":20},"seven_day_sonnet":{"utilization":100}}`, false},
		{"other Claude family does not block", "claude", "claude-opus-4", `{"five_hour":{"utilization":50},"seven_day":{"utilization":20},"seven_day_sonnet":{"utilization":100}}`, true},
		{"Anthropic alias", "anthropic", "claude-opus-4", `{"five_hour":{"utilization":50},"seven_day":{"utilization":20}}`, true},
		{"Codex both windows", "codex", "gpt-5.5", `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":50},"secondary_window":{"limit_window_seconds":604800,"used_percent":20}}}`, true},
		{"Codex missing percentage is unknown", "codex", "gpt-5.5", `{"rate_limit":{"primary_window":{"limit_window_seconds":18000},"secondary_window":{"limit_window_seconds":604800,"used_percent":50}}}`, false},
		{"Codex out of range percentage is unknown", "codex", "gpt-5.5", `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":-1},"secondary_window":{"limit_window_seconds":604800,"used_percent":50}}}`, false},
		{"monthly is not weekly", "codex", "gpt-5.5", `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":50},"secondary_window":{"limit_window_seconds":2592000,"used_percent":50}}}`, false},
		{"short window is not five-hour", "codex", "gpt-5.5", `{"rate_limit":{"primary_window":{"limit_window_seconds":3600,"used_percent":50},"secondary_window":{"limit_window_seconds":604800,"used_percent":50}}}`, false},
		{"Codex explicitly blocked", "codex", "gpt-5.5", `{"rate_limit":{"allowed":false,"primary_window":{"limit_window_seconds":18000,"used_percent":50},"secondary_window":{"limit_window_seconds":604800,"used_percent":50}}}`, false},
		{"Codex limit reached", "codex", "gpt-5.5", `{"rate_limit":{"limit_reached":true,"primary_window":{"limit_window_seconds":18000,"used_percent":50},"secondary_window":{"limit_window_seconds":604800,"used_percent":50}}}`, false},
		{"missing five-hour window", "claude", "claude-sonnet-4", `{"five_hour":null,"seven_day":{"utilization":20}}`, false},
		{"Claude missing percentage is unknown", "claude", "claude-sonnet-4", `{"five_hour":{},"seven_day":{"utilization":20}}`, false},
		{"Claude invalid percentage is unknown", "claude", "claude-sonnet-4", `{"five_hour":{"utilization":101},"seven_day":{"utilization":20}}`, false},
		{"scoped window cannot replace shared weekly", "claude", "claude-sonnet-4", `{"five_hour":{"utilization":50},"seven_day_sonnet":{"utilization":20}}`, false},
		{"Codex Spark limit blocks relevant model", "codex", "gpt-5.3-codex-spark", `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":50},"secondary_window":{"limit_window_seconds":604800,"used_percent":50}},"additional_rate_limits":[{"limit_name":"gpt-5.3-codex-spark","rate_limit":{"secondary_window":{"limit_window_seconds":604800,"used_percent":100}}}]}`, false},
		{"other Codex model ignores Spark limit", "codex", "gpt-5.5", `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":50},"secondary_window":{"limit_window_seconds":604800,"used_percent":50}},"additional_rate_limits":[{"metered_feature":"gpt-5.3-codex-spark","rate_limit":{"secondary_window":{"limit_window_seconds":604800,"used_percent":100}}}]}`, true},
		{"code-review limit does not block inference", "codex", "gpt-5.5", `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":50},"secondary_window":{"limit_window_seconds":604800,"used_percent":50}},"code_review_rate_limit":{"secondary_window":{"limit_window_seconds":604800,"used_percent":100}}}`, true},
		{"Fable scoped active limit blocks", "claude", "claude-fable-5", `{"five_hour":{"utilization":50},"seven_day":{"utilization":20},"limits":[{"kind":"weekly_scoped","scope":{"model":{"display_name":"Fable 5"}},"percent":0},{"kind":"weekly_scoped","scope":{"model":{"display_name":"Fable"}},"is_active":true,"percent":100}]}`, false},
		{"new Fable limit replaces legacy", "claude", "claude-fable-5", `{"five_hour":{"utilization":50},"seven_day":{"utilization":20},"iguana_necktie":{"utilization":100},"limits":[{"kind":"weekly_scoped","scope":{"model":{"display_name":"Fable"}},"is_active":true,"percent":0}]}`, true},
		{"Fable legacy limit", "claude", "claude-fable-5", `{"five_hour":{"utilization":50},"seven_day":{"utilization":20},"iguana_necktie":{"utilization":100}}`, false},
		{"dollar balance is not Fable weekly", "claude", "claude-fable-5", `{"five_hour":{"utilization":50},"seven_day":{"utilization":20},"iguana_necktie":{"used_dollars":10,"utilization":100}}`, true},
		{"missing rate limits", "codex", "gpt-5.5", `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := parseQuota([]byte(tc.body), tc.provider, testNow)
			if err != nil {
				t.Fatal(err)
			}
			_, _, usable := q.usable(tc.model, testNow, time.Minute)
			if usable != tc.usable {
				t.Fatalf("usable = %v, want %v", usable, tc.usable)
			}
		})
	}
}

func TestQuotaResetFields(t *testing.T) {
	for _, tc := range []struct {
		name, provider, body string
		five, week           time.Time
	}{
		{"Codex reversed windows use duration", "codex", `{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":20,"reset_at":1793286000},"secondary_window":{"limit_window_seconds":18000,"used_percent":50,"reset_at":1793282400000}}}`, testNow.Add(2 * time.Hour), testNow.Add(3 * time.Hour)},
		{"Codex relative reset", "codex", `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":50,"reset_after_seconds":3600},"secondary_window":{"limit_window_seconds":604800,"used_percent":20}}}`, testNow.Add(time.Hour), time.Time{}},
		{"absolute reset precedes relative", "codex", `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":50,"reset_at":"2026-10-29T13:00:00Z","reset_after_seconds":1},"secondary_window":{"limit_window_seconds":604800,"used_percent":20}}}`, testNow.Add(time.Hour), time.Time{}},
		{"Claude timestamps", "claude", `{"five_hour":{"utilization":50,"resets_at":"2026-10-29T13:00:00Z"},"seven_day":{"utilization":20,"resets_at":"2026-10-29T14:00:00Z"}}`, testNow.Add(time.Hour), testNow.Add(2 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := parseQuota([]byte(tc.body), tc.provider, testNow)
			if err != nil {
				t.Fatal(err)
			}
			five, week, ok := q.usable("gpt-5.5", testNow, time.Minute)
			if !ok || !five.Equal(tc.five) || !week.Equal(tc.week) {
				t.Fatalf("resets = %s, %s, usable = %v", five, week, ok)
			}
		})
	}
}

func TestMalformedUsageFails(t *testing.T) {
	for _, tc := range []struct{ provider, body string }{
		{"codex", `<html>blocked</html>`}, {"codex", `{"rate_limit":{"primary_window":{"used_percent":"50"}}}`},
		{"claude", `{"five_hour":{"utilization":"50"}}`}, {"claude", `{"limits":{}}`}, {"unsupported", `{}`},
	} {
		if _, err := parseQuota([]byte(tc.body), tc.provider, testNow); err == nil {
			t.Fatalf("malformed %s response accepted", tc.provider)
		}
	}
}
