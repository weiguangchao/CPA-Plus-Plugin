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
				{Name: "cpa_url", Type: pluginapi.ConfigFieldTypeString, Default: "http://127.0.0.1:8317", Description: "CPA 服务地址，可包含部署子路径，无需填写 /v8/management。"},
				{Name: "management_key", Type: pluginapi.ConfigFieldTypeString, Description: "直接填写 CPA 管理密钥。留空时不查询额度，调度交回 CPA。"},
				{Name: "cache_ttl", Type: pluginapi.ConfigFieldTypeString, Default: "5m", Description: "已选账号缓存的最长有效期。到期后由新请求触发刷新；到达已知额度重置时间时可提前失效。默认 5m，即 5 分钟。"},
				{Name: "request_timeout", Type: pluginapi.ConfigFieldTypeString, Default: "10s", Description: "每次 CPA 管理 API 请求的超时时间，也是调度在无有效缓存时等待查询结果的上限，包含排队时间。默认 10s，即 10 秒。等待超时后交回 CPA，后台查询继续；不限制推理执行时间。"},
				{Name: "retry_interval", Type: pluginapi.ConfigFieldTypeString, Default: "20s", Description: "查询失败或未选出账号后，允许新请求再次查询前的等待时间。默认 20s，即 20 秒；已知重置时间更早时可提前重试，没有请求时不重试。"},
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
