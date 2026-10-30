package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var (
	version     = "dev"
	author      = "weiguangchao"
	repository  = "https://github.com/weiguangchao/CPA-Plus-Plugin"
	active      atomic.Pointer[engine]
	lifecycleMu sync.Mutex
)

func main() {}

func handleMethod(method string, raw []byte) (response []byte, err error) {
	defer func() {
		if recover() != nil {
			if method == pluginabi.MethodSchedulerPick {
				response, err = okEnvelope(pluginapi.SchedulerPickResponse{})
			} else {
				response, err = nil, fmt.Errorf("plugin call panicked")
			}
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var request struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, fmt.Errorf("decode lifecycle request: %w", err)
		}
		cfg, err := parseConfig(request.ConfigYAML)
		if err != nil {
			return nil, err
		}
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		next := newEngine(cfg)
		if previous := active.Swap(next); previous != nil {
			previous.stop()
		}
		next.start()
		return okEnvelope(registration())
	case pluginabi.MethodPluginQuiesce:
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		if current := active.Load(); current != nil {
			current.stop()
		}
		return okEnvelope(struct{}{})
	case pluginabi.MethodPluginShutdown:
		shutdown()
		return okEnvelope(struct{}{})
	case pluginabi.MethodSchedulerPick:
		var request pluginapi.SchedulerPickRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, fmt.Errorf("decode scheduler request: %w", err)
		}
		if current := active.Load(); current != nil {
			return okEnvelope(current.pick(request))
		}
		return okEnvelope(pluginapi.SchedulerPickResponse{})
	default:
		return nil, fmt.Errorf("unsupported plugin method")
	}
}

func shutdown() {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if previous := active.Swap(nil); previous != nil {
		previous.stop()
	}
}

func registration() any {
	return struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
		Capabilities  struct {
			Scheduler        bool `json:"scheduler"`
			AcrossPriorities bool `json:"scheduler_across_priorities"`
		} `json:"capabilities"`
	}{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name: "CPA Plus", Version: strings.TrimSpace(version),
			Author: author, GitHubRepository: repository,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "cpa_url", Type: pluginapi.ConfigFieldTypeString, Default: "http://127.0.0.1:8317", Description: "CPA server URL, optionally including a deployment subpath."},
				{Name: "management_key", Type: pluginapi.ConfigFieldTypeString, Description: "CPA management key entered directly. Leave empty to delegate scheduling to CPA without quota queries."},
				{Name: "cache_ttl", Type: pluginapi.ConfigFieldTypeString, Default: "5m", Description: "Maximum age of a cached account selection before an on-demand refresh."},
				{Name: "retry_interval", Type: pluginapi.ConfigFieldTypeString, Default: "20s", Description: "Delay after an unsuccessful quota selection before a new scheduling request can retry. A known earlier reset can shorten this delay. No periodic retries."},
				{Name: "request_timeout", Type: pluginapi.ConfigFieldTypeString, Default: "10s", Description: "Timeout for each background CPA management API request, not inference requests or the entire refresh."},
				{Name: "providers", Type: pluginapi.ConfigFieldTypeArray, Default: []string{"claude", "codex"}, Description: "Provider keys whose quotas participate in plugin scheduling."},
			},
		},
		Capabilities: struct {
			Scheduler        bool `json:"scheduler"`
			AcrossPriorities bool `json:"scheduler_across_priorities"`
		}{Scheduler: true, AcrossPriorities: true},
	}
}

func okEnvelope(value any) ([]byte, error) {
	result, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: result})
}
