package main

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const pluginID = "cpa-plus"

type config struct {
	CPAURL         string        `yaml:"cpa_url"`
	ManagementKey  string        `yaml:"management_key"`
	CacheTTL       time.Duration `yaml:"cache_ttl"`
	RetryInterval  time.Duration `yaml:"retry_interval"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
	Providers      []string      `yaml:"providers"`
}

func parseConfig(raw []byte) (config, error) {
	cfg := config{
		CPAURL:        "http://127.0.0.1:8317",
		CacheTTL:      5 * time.Minute,
		RetryInterval: 20 * time.Second, RequestTimeout: 10 * time.Second,
		Providers: []string{"claude", "codex"},
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := yaml.NewDecoder(bytes.NewReader(raw)).Decode(&cfg); err != nil {
			return config{}, fmt.Errorf("decode plugin configuration: %w", err)
		}
	}
	u, err := url.Parse(strings.TrimSpace(cfg.CPAURL))
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return config{}, fmt.Errorf("cpa_url must be an HTTP(S) URL without credentials, query or fragment")
	}
	cfg.CPAURL = strings.TrimRight(u.String(), "/")
	if cfg.RetryInterval < time.Second || cfg.RequestTimeout < time.Second || cfg.CacheTTL < time.Second {
		return config{}, fmt.Errorf("cache_ttl, retry_interval and request_timeout must be at least 1s")
	}
	if len(cfg.Providers) == 0 {
		return config{}, fmt.Errorf("providers must contain at least one provider")
	}
	for i, provider := range cfg.Providers {
		cfg.Providers[i] = strings.ToLower(strings.TrimSpace(provider))
		switch cfg.Providers[i] {
		case "claude", "anthropic", "codex":
		default:
			return config{}, fmt.Errorf("providers must contain only claude, anthropic or codex")
		}
	}
	if strings.ContainsAny(cfg.ManagementKey, "\r\n") {
		return config{}, fmt.Errorf("management_key must not contain line breaks")
	}
	cfg.ManagementKey = strings.TrimSpace(cfg.ManagementKey)
	return cfg, nil
}

func (c config) manages(provider string) bool {
	for _, allowed := range c.Providers {
		if allowed == provider {
			return true
		}
	}
	return false
}
