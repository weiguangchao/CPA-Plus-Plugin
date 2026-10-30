package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUsageRequestsThroughCPA(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			calls := 0
			c := credential{ID: "a", Index: "index-a", Provider: provider}
			c.IDToken.AccountID = "account-a"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/v8/management/requests/api-call" || r.Header.Get("Authorization") != "Bearer test-management-key" {
					t.Errorf("unexpected management request: %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var request struct {
					Index  string            `json:"auth_index"`
					Method string            `json:"method"`
					URL    string            `json:"url"`
					Header map[string]string `json:"header"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if request.Index != "index-a" || request.Method != "GET" || request.Header["Authorization"] != "Bearer $TOKEN$" {
					t.Errorf("invalid forwarded request: %+v", request)
				}
				body := quotaJSON(.5, .5, testNow.Add(time.Hour).Unix(), nil)
				if provider == "codex" {
					if request.URL != "https://chatgpt.com/backend-api/wham/usage" || request.Header["Chatgpt-Account-Id"] != "account-a" {
						t.Errorf("invalid Codex request: %+v", request)
					}
				} else {
					if request.URL != "https://api.anthropic.com/api/oauth/usage" || request.Header["anthropic-beta"] != "oauth-2025-04-20" || request.Header["Chatgpt-Account-Id"] != "" {
						t.Errorf("invalid Claude request: %+v", request)
					}
					body = []byte(`{"five_hour":{"utilization":50,"resets_at":"2026-10-29T13:00:00Z"},"seven_day":{"utilization":20}}`)
				}
				if request.Header["User-Agent"] == "" {
					t.Error("missing upstream user agent")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": string(body)})
			}))
			defer server.Close()
			a := newManagementAPI(config{CPAURL: server.URL, RequestTimeout: time.Second, ManagementKey: "test-management-key"})
			defer a.http.CloseIdleConnections()
			q, err := a.quota(context.Background(), c, testNow)
			if err != nil {
				t.Fatal(err)
			}
			five, _, ok := q.usable("gpt-5.5", testNow, time.Minute)
			if !ok || !five.Equal(testNow.Add(time.Hour)) || calls != 1 {
				t.Fatalf("usable=%v reset=%s calls=%d", ok, five, calls)
			}
		})
	}
}

func TestAPICallFailuresDoNotProduceQuota(t *testing.T) {
	valid := string(quotaJSON(.5, .5, testNow.Add(time.Hour).Unix(), nil))
	for _, tc := range []struct {
		name   string
		status int
		raw    string
	}{
		{"upstream unauthorized", 200, `{"status_code":401,"body":` + quoted(valid) + `}`},
		{"upstream limited", 200, `{"status_code":429,"body":` + quoted(valid) + `}`},
		{"upstream unavailable", 200, `{"status_code":503,"body":` + quoted(valid) + `}`},
		{"missing upstream status", 200, `{"body":` + quoted(valid) + `}`},
		{"malformed envelope", 200, `<html>login</html>`},
		{"body is not a string", 200, `{"status_code":200,"body":{}}`},
		{"malformed usage", 200, `{"status_code":200,"body":"upstream-secret-body"}`},
		{"management failure", 502, `upstream-secret-body`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); _, _ = w.Write([]byte(tc.raw)) }))
			defer server.Close()
			a := newManagementAPI(config{CPAURL: server.URL, RequestTimeout: time.Second})
			defer a.http.CloseIdleConnections()
			_, err := a.quota(context.Background(), credential{Index: "a", Provider: "codex"}, testNow)
			if err == nil {
				t.Fatal("failed query accepted")
			}
			if strings.Contains(err.Error(), "upstream-secret-body") {
				t.Fatal("upstream body included in error")
			}
		})
	}
}

func quoted(s string) string { raw, _ := json.Marshal(s); return string(raw) }

func TestSIWCUsesOnlyNestedCredential(t *testing.T) {
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1793282400}`))
	jwt := "header." + claims + ".signature"
	for _, tc := range []struct {
		name, file, filename, token string
		downloadStatus              int
		usable                      bool
	}{
		{"valid", `{"access_token":"outer-token","siwc":{"access_token":"nested-token","expires_at":"2026-10-29T13:00:00Z"}}`, "account a.json", "nested-token", 200, true},
		{"JWT expiry", `{"siwc":{"access_token":` + quoted(jwt) + `}}`, "a.json", jwt, 200, true},
		{"expired", `{"access_token":"outer-token","siwc":{"access_token":"nested-token","expires_at":"2026-10-29T11:00:00Z"}}`, "a.json", "", 200, false},
		{"missing nested token", `{"access_token":"outer-token"}`, "a.json", "", 200, false},
		{"unknown expiry", `{"siwc":{"access_token":"nested-token"}}`, "a.json", "", 200, false},
		{"download failure", `{}`, "a.json", "", 404, false},
		{"malformed file", `<html>secret</html>`, "a.json", "", 200, false},
		{"runtime-only", `{}`, "", "", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queries := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-management-key" {
					t.Error("missing management authorization")
					w.WriteHeader(401)
					return
				}
				if r.Method == http.MethodGet && r.URL.Path == "/v8/management/credentials/download" {
					if r.URL.Query().Get("name") != tc.filename {
						t.Error("wrong credential file")
					}
					w.WriteHeader(tc.downloadStatus)
					_, _ = w.Write([]byte(tc.file))
					return
				}
				queries++
				if r.Method != http.MethodPost || r.URL.Path != "/v8/management/requests/api-call" {
					t.Error("unexpected quota route")
					w.WriteHeader(404)
					return
				}
				var request struct {
					Index  string            `json:"auth_index"`
					Header map[string]string `json:"header"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if request.Index != "index-a" || request.Header["Authorization"] != "Bearer "+tc.token || request.Header["Chatgpt-Account-Id"] != "" {
					t.Error("SIWC request used the wrong account or credential")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": string(quotaJSON(.5, .5, testNow.Add(time.Hour).Unix(), nil))})
			}))
			defer server.Close()
			a := newManagementAPI(config{CPAURL: server.URL, RequestTimeout: time.Second, ManagementKey: "test-management-key"})
			defer a.http.CloseIdleConnections()
			c := credential{ID: "a", Index: "index-a", Name: tc.filename, Provider: "codex", UseSIWC: true}
			c.IDToken.AccountID = "outer-account"
			_, err := a.quota(context.Background(), c, testNow)
			if (err == nil) != tc.usable {
				t.Fatalf("error = %v, usable = %v", err, tc.usable)
			}
			want := 0
			if tc.usable {
				want = 1
			}
			if queries != want {
				t.Fatalf("upstream queries = %d, want %d", queries, want)
			}
		})
	}
}
