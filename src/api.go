package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type authIndex string
type credential struct {
	ID       authID    `json:"id"`
	Index    authIndex `json:"auth_index"`
	Name     string    `json:"name"`
	Provider string    `json:"provider"`
	Disabled bool      `json:"disabled"`
	UseSIWC  bool      `json:"use_siwc"`
	IDToken  struct {
		AccountID string `json:"chatgpt_account_id"`
	} `json:"id_token"`
}

type managementAPI struct {
	baseURL string
	key     string
	http    *http.Client
}

func newManagementAPI(cfg config) *managementAPI {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &managementAPI{
		baseURL: cfg.CPAURL + "/v8/management", key: cfg.ManagementKey,
		http: &http.Client{Timeout: cfg.RequestTimeout, Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

func (a *managementAPI) request(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create CPA request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.key)
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("CPA management request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CPA management returned HTTP %d", resp.StatusCode)
	}
	const maxResponseBytes = 16 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read CPA management response: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("CPA management response exceeds 16 MiB")
	}
	return raw, nil
}

func (a *managementAPI) credentials(ctx context.Context) ([]credential, error) {
	raw, err := a.request(ctx, http.MethodGet, "/credentials", nil)
	if err != nil {
		return nil, err
	}
	var response struct {
		Files *[]credential `json:"files"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || response.Files == nil {
		return nil, fmt.Errorf("CPA credentials response must contain a files array")
	}
	for i := range *response.Files {
		(*response.Files)[i].Provider = strings.ToLower(strings.TrimSpace((*response.Files)[i].Provider))
	}
	return *response.Files, nil
}

func (a *managementAPI) quota(ctx context.Context, c credential, now time.Time) (quotaSnapshot, error) {
	headers := map[string]string{"Authorization": "Bearer $TOKEN$", "Accept": "application/json"}
	var upstream string
	switch c.Provider {
	case "codex":
		upstream = "https://chatgpt.com/backend-api/wham/usage"
		headers["User-Agent"] = "codex-tui/0.149.1"
		if c.UseSIWC {
			token, err := a.siwcToken(ctx, c, now)
			if err != nil {
				return quotaSnapshot{}, err
			}
			headers["Authorization"] = "Bearer " + token
		} else if id := strings.TrimSpace(c.IDToken.AccountID); id != "" {
			headers["Chatgpt-Account-Id"] = id
		}
	case "claude", "anthropic":
		upstream = "https://api.anthropic.com/api/oauth/usage"
		headers["anthropic-beta"] = "oauth-2025-04-20"
		headers["User-Agent"] = "claude-cli/2.1.280 (external, cli)"
	default:
		return quotaSnapshot{}, fmt.Errorf("unsupported quota provider")
	}
	body, err := json.Marshal(struct {
		Index  authIndex         `json:"auth_index"`
		Method string            `json:"method"`
		URL    string            `json:"url"`
		Header map[string]string `json:"header"`
	}{c.Index, http.MethodGet, upstream, headers})
	if err != nil {
		return quotaSnapshot{}, err
	}
	raw, err := a.request(ctx, http.MethodPost, "/requests/api-call", body)
	if err != nil {
		return quotaSnapshot{}, err
	}
	var response struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return quotaSnapshot{}, fmt.Errorf("decode CPA API-call response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return quotaSnapshot{}, fmt.Errorf("quota upstream returned HTTP %d", response.StatusCode)
	}
	return parseQuota([]byte(response.Body), c.Provider, now)
}

func (a *managementAPI) siwcToken(ctx context.Context, c credential, now time.Time) (string, error) {
	if c.Name == "" || strings.ContainsAny(c.Name, "/\\") || !strings.HasSuffix(strings.ToLower(c.Name), ".json") {
		return "", fmt.Errorf("SIWC quota requires a downloadable credential")
	}
	raw, err := a.request(ctx, http.MethodGet, "/credentials/download?name="+url.QueryEscape(c.Name), nil)
	if err != nil {
		return "", err
	}
	var stored struct {
		SIWC struct {
			Token        string          `json:"access_token"`
			ExpiresAt    json.RawMessage `json:"expires_at"`
			ExpiresCamel json.RawMessage `json:"expiresAt"`
		} `json:"siwc"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return "", fmt.Errorf("decode SIWC credential")
	}
	token := strings.TrimSpace(stored.SIWC.Token)
	expires := parseResetTime(stored.SIWC.ExpiresAt)
	if expires.IsZero() {
		expires = parseResetTime(stored.SIWC.ExpiresCamel)
	}
	if expires.IsZero() {
		parts := strings.Split(token, ".")
		if len(parts) == 3 {
			payload, err := base64.RawURLEncoding.DecodeString(parts[1])
			var claims struct {
				Exp json.RawMessage `json:"exp"`
			}
			if err == nil && json.Unmarshal(payload, &claims) == nil {
				expires = parseResetTime(claims.Exp)
			}
		}
	}
	if token == "" || strings.ContainsAny(token, "\r\n") || !expires.After(now) {
		return "", fmt.Errorf("SIWC credential is missing or expired")
	}
	return token, nil
}
