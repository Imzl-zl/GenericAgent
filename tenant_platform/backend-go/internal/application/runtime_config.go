package application

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

const (
	runtimeConfigFilename = "mykey.runtime.json"
	myKeyLoaderFilename   = "mykey.py"
)

const MyKeyLoader = `import json as _json
from pathlib import Path as _Path
_config = _json.loads(_Path(__file__).with_name("mykey.runtime.json").read_text(encoding="utf-8"))
globals().update(_config)
del _config
`

// RuntimeProviderBinding 是单个 provider × 能力维度的运行时绑定: chat 能力
// 写 session 变量(进 mixin), image 能力写 image_gen 块(不进 mixin)。
type RuntimeProviderBinding struct {
	Provider domain.LLMProvider
	Token    string
	// Capability 该 binding 对应的能力维度(chat|image)。
	Capability domain.ProviderCapability
}

type RuntimeMCPServer struct {
	ServerID       string `json:"server_id"`
	Name           string `json:"name"`
	URL            string `json:"url"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	// Transport 接入方式(http | stdio); 省略 = http。stdio 服务器由
	// Worker 沙箱内进程宿主拉起, 不需要 url/proxy。
	Transport string   `json:"transport,omitempty"`
	Command   string   `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`
}

type RuntimeMCPSnapshot struct {
	ID      string             `json:"snapshot_id"`
	Servers []RuntimeMCPServer `json:"servers"`
	// Proxy 非空时, Worker 经 Platform 受控 MCP proxy 访问外部 HTTP MCP
	// Server(key 平台侧持有 + 配额计量): server_id → URL 映射即白名单。
	Proxy *RuntimeMCPProxy `json:"proxy,omitempty"`
}

// RuntimeMCPProxy 是 Worker 可用的 MCP proxy 端点(带短期 capability)。
type RuntimeMCPProxy struct {
	BaseURL         string `json:"base_url"`
	CapabilityToken string `json:"capability_token"`
}

type RuntimeConfigInput struct {
	ProxyBaseURL      string
	RoutingSnapshotID string
	Providers         []RuntimeProviderBinding
	MCP               RuntimeMCPSnapshot
	// JTIs 随凭证集写入运行时配置, 供 Worker 校验 capability_jti 归属。
	JTIs []string
	// Sophub proxy capability(方案 §5.2): 非空时写入 _platform_sophub,
	// Worker 经 Platform 受控 proxy 搜索/安装 SOP, 不持有 Sophub API Key。
	Sophub *RuntimeSophubProxy
}

// RuntimeSophubProxy 是 Worker 可用的 Sophub proxy 端点(带短期 capability)。
type RuntimeSophubProxy struct {
	BaseURL         string `json:"base_url"`
	CapabilityToken string `json:"capability_token"`
}

type RuntimeConfigMetadata struct {
	RoutingSnapshotID string `json:"routing_snapshot_id"`
	// JTIs 是本凭证集签发的全部 capability token JTI(方案 §7 per-task
	// capability): Worker 校验 ExecuteTask 的 capability_jti 必须属于当前
	// 集合, 防止任意非空 JTI 通过(审查: Worker 身份与 capability 校验)。
	JTIs []string `json:"jtis,omitempty"`
}

type RuntimeConfigFiles struct {
	JSON       []byte
	Loader     []byte
	SnapshotID string
}

func BuildRuntimeConfig(input RuntimeConfigInput) (RuntimeConfigFiles, error) {
	if strings.TrimSpace(input.RoutingSnapshotID) == "" {
		return RuntimeConfigFiles{}, fmt.Errorf("routing snapshot id is required")
	}
	proxyBase, err := parseProxyBase(input.ProxyBaseURL)
	if err != nil {
		return RuntimeConfigFiles{}, err
	}
	if len(input.Providers) == 0 {
		return RuntimeConfigFiles{}, fmt.Errorf("at least one provider is required")
	}

	document := make(map[string]any, len(input.Providers)+3)
	metadata := RuntimeConfigMetadata{
		RoutingSnapshotID: input.RoutingSnapshotID,
		JTIs:              append([]string(nil), input.JTIs...),
	}
	document["_platform_runtime"] = metadata
	if strings.TrimSpace(input.MCP.ID) != "" {
		if err := validateRuntimeMCPSnapshot(input.MCP); err != nil {
			return RuntimeConfigFiles{}, err
		}
		document["_platform_mcp"] = input.MCP
	}
	if input.Sophub != nil {
		if strings.TrimSpace(input.Sophub.BaseURL) == "" || strings.TrimSpace(input.Sophub.CapabilityToken) == "" {
			return RuntimeConfigFiles{}, fmt.Errorf("sophub proxy base_url and capability_token are required")
		}
		document["_platform_sophub"] = map[string]any{
			"base_url":         strings.TrimRight(strings.TrimSpace(input.Sophub.BaseURL), "/"),
			"capability_token": strings.TrimSpace(input.Sophub.CapabilityToken),
		}
	}
	// bindingKey 按 (provider ID, 能力维度) 去重——双能力 provider(chat+image)
	// 签两个 token 产生两个 binding, 同 ID 不同能力是合法组合(审查 S4/W6
	// 补测暴露: 原按 ID 去重会让双能力 provider 的 runtime config 必然失败)。
	type bindingKey struct {
		id  int64
		cap domain.ProviderCapability
	}
	seen := make(map[bindingKey]struct{}, len(input.Providers))
	mixinNames := make([]string, 0, len(input.Providers))
	imageBound := false
	chatBound := false
	for _, binding := range input.Providers {
		provider := binding.Provider
		if err := validateRuntimeBinding(binding); err != nil {
			return RuntimeConfigFiles{}, err
		}
		effectiveCap := binding.Capability
		if effectiveCap == "" {
			// 兼容存量构造(binding 未标能力): 按 provider 能力自动归类。
			if provider.HasCapability(domain.ProviderCapabilityImage) {
				effectiveCap = domain.ProviderCapabilityImage
			} else {
				effectiveCap = domain.ProviderCapabilityChat
			}
		}
		key := bindingKey{provider.ID, effectiveCap}
		if _, exists := seen[key]; exists {
			return RuntimeConfigFiles{}, fmt.Errorf("duplicate provider id %d for capability %q", provider.ID, effectiveCap)
		}
		seen[key] = struct{}{}
		// Phase B 托管形态(2026-08-14 定稿): image 能力 binding 写 image_gen
		// 块(GA resolve_image_gen 读取), **不进 chat mixin**——生图是角色
		// 分离不是同能力故障转移。v1 只支持单 image provider(fail-closed)。
		if binding.Capability == domain.ProviderCapabilityImage ||
			(binding.Capability == "" && provider.HasCapability(domain.ProviderCapabilityImage)) {
			if imageBound {
				return RuntimeConfigFiles{}, fmt.Errorf("multiple image-capable providers are not supported yet")
			}
			if provider.ProviderType != domain.ProviderNativeOAI {
				return RuntimeConfigFiles{}, fmt.Errorf("image capability requires native_oai provider")
			}
			document["image_gen"] = map[string]any{
				"name":        "openai",
				"apibase":     strings.TrimRight(proxyBase.String(), "/") + "/v1",
				"apikey":      binding.Token,
				"model":       provider.Model,
				"stream":      false,
				"max_retries": 2,
			}
			imageBound = true
			continue
		}
		variableName := runtimeProviderVariable(provider)
		runtimeName := runtimeProviderName(provider.ID)
		config, err := buildRuntimeProviderConfig(proxyBase, binding, runtimeName)
		if err != nil {
			return RuntimeConfigFiles{}, err
		}
		document[variableName] = config
		mixinNames = append(mixinNames, runtimeName)
		chatBound = true
	}
	// 审查 W6: 至少一个 chat 能力 provider——image-only 部署(全部 provider
	// 仅 image)会产出无对话 session 的 runtime config, GA 沙箱无对话模型、
	// 任务必然失败且无清晰报错。fail-fast 于签发时刻。
	if !chatBound {
		return RuntimeConfigFiles{}, fmt.Errorf("at least one chat-capable provider is required")
	}
	if len(mixinNames) > 1 {
		document["mixin_config"] = map[string]any{"llm_nos": mixinNames}
	}

	encoded, err := marshalRuntimeDocument(document)
	if err != nil {
		return RuntimeConfigFiles{}, err
	}
	return RuntimeConfigFiles{
		JSON: encoded, Loader: []byte(MyKeyLoader), SnapshotID: input.RoutingSnapshotID,
	}, nil
}

func WriteRuntimeConfigAtomic(configRoot string, files RuntimeConfigFiles) error {
	if strings.TrimSpace(configRoot) == "" {
		return fmt.Errorf("config root is required")
	}
	if len(files.JSON) == 0 || !bytes.Equal(files.Loader, []byte(MyKeyLoader)) {
		return fmt.Errorf("runtime config files are incomplete")
	}
	// 0640/0770: 生产 Runner 模式写入共享卷 config/(setgid 10003), Runner
	// 以附加组 10003 读取; loopback 下 ConfigRoot 目录私有不受影响(审查
	// Blocker#3: 0600 使复用 Runner 的刷新文件对 Runner 不可读)。
	if err := os.MkdirAll(configRoot, 0o770); err != nil {
		return fmt.Errorf("create config root: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(configRoot, runtimeConfigFilename), files.JSON, 0o640); err != nil {
		return fmt.Errorf("write runtime JSON: %w", err)
	}
	loaderPath := filepath.Join(configRoot, myKeyLoaderFilename)
	if current, err := os.ReadFile(loaderPath); err == nil && bytes.Equal(current, files.Loader) {
		// loader 内容固定不变; 任务即进程下每任务冷启动必读新配置, touch
		// 保证 GA 原生 reload_mykeys 的 mtime 检测看到最新写入(与写配置
		// 原子性配合, 防止陈旧 mtime 缓存)。
		now := time.Now()
		if err := os.Chtimes(loaderPath, now, now); err != nil {
			return fmt.Errorf("touch mykey loader: %w", err)
		}
		return nil
	}
	if err := writeFileAtomic(loaderPath, files.Loader, 0o640); err != nil {
		return fmt.Errorf("write mykey loader: %w", err)
	}
	return nil
}

func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(content); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return replaceFileAtomic(tempPath, path)
}

func validateRuntimeBinding(binding RuntimeProviderBinding) error {
	provider := binding.Provider
	if provider.ID <= 0 || provider.Revision <= 0 {
		return fmt.Errorf("provider id and revision must be positive")
	}
	if binding.Token == "" {
		return fmt.Errorf("provider %d capability token is required", provider.ID)
	}
	if provider.Model == "" {
		return fmt.Errorf("provider %d model is required", provider.ID)
	}
	return provider.SessionConfig.Validate(provider.ProviderType)
}

func validateRuntimeMCPSnapshot(snapshot RuntimeMCPSnapshot) error {
	if strings.TrimSpace(snapshot.ID) == "" {
		return fmt.Errorf("MCP snapshot id is required")
	}
	seen := make(map[string]struct{}, len(snapshot.Servers))
	for _, server := range snapshot.Servers {
		if _, duplicate := seen[server.ServerID]; duplicate {
			return fmt.Errorf("duplicate MCP server id %q", server.ServerID)
		}
		seen[server.ServerID] = struct{}{}
		if err := domain.ValidateMCPServerInput(domain.MCPServerCreate{
			ServerKey: server.ServerID, Name: server.Name, URL: server.URL,
			TimeoutSeconds: server.TimeoutSeconds,
			Transport:      server.Transport, Command: server.Command, Args: server.Args,
		}); err != nil {
			return fmt.Errorf("MCP server %q: %w", server.ServerID, err)
		}
	}
	if snapshot.Proxy != nil {
		if strings.TrimSpace(snapshot.Proxy.BaseURL) == "" || strings.TrimSpace(snapshot.Proxy.CapabilityToken) == "" {
			return fmt.Errorf("MCP proxy base_url and capability_token are required")
		}
		if _, err := parseProxyBase(snapshot.Proxy.BaseURL); err != nil {
			return fmt.Errorf("MCP proxy base_url: %w", err)
		}
	}
	return nil
}

func parseProxyBase(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("proxy base URL must be absolute")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("proxy base URL scheme must be http or https")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, fmt.Errorf("proxy base URL must not contain userinfo, query, or fragment")
	}
	return parsed, nil
}

func runtimeProviderVariable(provider domain.LLMProvider) string {
	return "platform_" + string(provider.ProviderType) + "_provider_" + strconv.FormatInt(provider.ID, 10) + "_config"
}

func runtimeProviderName(id int64) string {
	return "provider-" + strconv.FormatInt(id, 10)
}

func buildRuntimeProviderConfig(proxyBase *url.URL, binding RuntimeProviderBinding, runtimeName string) (map[string]any, error) {
	provider := binding.Provider
	apibase := strings.TrimRight(proxyBase.String(), "/")
	if provider.ProviderType == domain.ProviderNativeOAI {
		apibase += "/v1"
	}
	encoded, err := json.Marshal(provider.SessionConfig)
	if err != nil {
		return nil, fmt.Errorf("marshal provider %d session config: %w", provider.ID, err)
	}
	config := make(map[string]any)
	if err := json.Unmarshal(encoded, &config); err != nil {
		return nil, fmt.Errorf("decode provider %d session config: %w", provider.ID, err)
	}
	config["name"] = runtimeName
	config["apikey"] = binding.Token
	config["apibase"] = apibase
	config["model"] = provider.Model
	return config, nil
}

func marshalRuntimeDocument(document map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(document))
	for key := range document {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for index, key := range keys {
		if index > 0 {
			buffer.WriteByte(',')
		}
		keyJSON, _ := json.Marshal(key)
		valueJSON, err := json.Marshal(document[key])
		if err != nil {
			return nil, fmt.Errorf("marshal runtime key %s: %w", key, err)
		}
		buffer.Write(keyJSON)
		buffer.WriteByte(':')
		buffer.Write(valueJSON)
	}
	buffer.WriteByte('}')
	buffer.WriteByte('\n')
	return buffer.Bytes(), nil
}
