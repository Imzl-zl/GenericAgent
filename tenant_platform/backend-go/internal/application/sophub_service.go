package application

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/secret"
)

const maxSophubAPIKeyBytes = 4096

type SophubClient interface {
	Verify(ctx context.Context, apiKey string) (domain.SophubIdentity, error)
	Search(ctx context.Context, apiKey, query string, page, pageSize int) (domain.SophubSearchResult, error)
	GetSOP(ctx context.Context, apiKey, remoteSOPID string) (domain.SophubRemoteSOP, error)
}

type SophubStore interface {
	UpsertSophubBinding(ctx context.Context, binding domain.SophubBinding) (domain.SophubBinding, error)
	GetSophubBinding(ctx context.Context) (domain.SophubBinding, error)
}

type SophubService interface {
	Bind(ctx context.Context, apiKey string, adminUserID int64) (domain.SophubBindingStatus, error)
	GetBindingStatus(ctx context.Context) (domain.SophubBindingStatus, error)
	Search(ctx context.Context, query string, page, pageSize int) (domain.SophubSearchResult, error)
	// FetchRemoteSOP 供 Worker proxy 使用: 返回远程 SOP 内容(不落任何注册表)。
	FetchRemoteSOP(ctx context.Context, remoteSOPID string) (domain.SophubRemoteSOP, error)
}

type SophubServiceConfig struct {
	Store  SophubStore
	Client SophubClient
	Cipher secret.TokenCipher
}

type sophubService struct {
	store  SophubStore
	client SophubClient
	cipher secret.TokenCipher
}

func NewSophubService(config SophubServiceConfig) (SophubService, error) {
	if config.Store == nil || config.Client == nil || config.Cipher == nil {
		return nil, fmt.Errorf("Sophub store, client, and cipher are required")
	}
	return &sophubService{store: config.Store, client: config.Client, cipher: config.Cipher}, nil
}

func (service *sophubService) Bind(ctx context.Context, apiKey string, adminUserID int64) (domain.SophubBindingStatus, error) {
	apiKey = strings.TrimSpace(apiKey)
	if err := validateSophubAPIKey(apiKey); err != nil {
		return domain.SophubBindingStatus{}, err
	}
	if adminUserID <= 0 {
		return domain.SophubBindingStatus{}, fmt.Errorf("admin user id must be positive")
	}
	identity, err := service.client.Verify(ctx, apiKey)
	if err != nil {
		return domain.SophubBindingStatus{}, fmt.Errorf("Sophub verification failed")
	}
	ciphertext, keyVersion, err := service.cipher.Encrypt([]byte(apiKey))
	if err != nil {
		return domain.SophubBindingStatus{}, fmt.Errorf("encrypt Sophub API key: %w", err)
	}
	binding, err := service.store.UpsertSophubBinding(ctx, domain.SophubBinding{
		APIKeyCiphertext: ciphertext,
		APIKeyVersion:    keyVersion,
		Identity:         identity,
		UpdatedBy:        adminUserID,
	})
	if err != nil {
		return domain.SophubBindingStatus{}, err
	}
	return binding.Status(), nil
}

func (service *sophubService) GetBindingStatus(ctx context.Context) (domain.SophubBindingStatus, error) {
	binding, err := service.store.GetSophubBinding(ctx)
	if err != nil {
		return domain.SophubBindingStatus{}, err
	}
	return binding.Status(), nil
}

func (service *sophubService) Search(ctx context.Context, query string, page, pageSize int) (domain.SophubSearchResult, error) {
	query = strings.TrimSpace(query)
	if !utf8.ValidString(query) || len([]byte(query)) > 200 {
		return domain.SophubSearchResult{}, fmt.Errorf("Sophub search query is invalid")
	}
	apiKey, err := service.bindingKey(ctx)
	if err != nil {
		return domain.SophubSearchResult{}, err
	}
	result, err := service.client.Search(ctx, apiKey, query, page, pageSize)
	if err != nil {
		return domain.SophubSearchResult{}, fmt.Errorf("Sophub search failed")
	}
	// 仅可安装项对用户/Worker 可见(方案 §5.2): 私有/未审核/非单文件/非
	// Markdown 的 SOP 不下发到任何工作区(审查: 与 FetchRemoteSOP 同一准入)。
	result.Items = filterInstallableSOPs(result.Items)
	result.Total = len(result.Items)
	return result, nil
}

// filterInstallableSOPs 只保留可安装 SOP(审查: 搜索与安装准入一致,
// 私有项目可能含租户敏感内容)。
func filterInstallableSOPs(items []domain.SophubRemoteSOP) []domain.SophubRemoteSOP {
	out := items[:0]
	for _, item := range items {
		if isSophubSOPPublic(item) &&
			strings.EqualFold(item.Status, domain.SOPStatusApproved) &&
			strings.EqualFold(item.PackageType, domain.SOPPackageTypeSingle) &&
			strings.EqualFold(item.FileType, domain.SOPFileTypeMarkdown) {
			out = append(out, item)
		}
	}
	return out
}

// SophubSourceCommunity 是远程公开 SOP 的发布源(社区分享)。
const SophubSourceCommunity = "community"

// isSophubSOPPublic 判定远程 SOP 的公开性: 远程 API(fudankw.cn/sophub)
// 不返回 is_public 字段(2026-08 实测列表与详情均无), 公开 SOP 以
// source=community(社区分享)表达。is_public 显式存在时以其为准
// (显式 false 拒绝——字段未来恢复时的 fail-closed); 缺失时以
// source=community 作为公开信号。
func isSophubSOPPublic(item domain.SophubRemoteSOP) bool {
	if item.IsPublic != nil {
		return *item.IsPublic
	}
	return item.Source == SophubSourceCommunity
}

// FetchRemoteSOP 返回远程 SOP(供 Worker proxy; 不写入任何注册表/候选)。
func (service *sophubService) FetchRemoteSOP(ctx context.Context, remoteSOPID string) (domain.SophubRemoteSOP, error) {
	remoteSOPID = strings.TrimSpace(remoteSOPID)
	if !utf8.ValidString(remoteSOPID) || len([]byte(remoteSOPID)) > domain.MaxSOPRemoteIDBytes {
		return domain.SophubRemoteSOP{}, fmt.Errorf("remote SOP id is invalid")
	}
	apiKey, err := service.bindingKey(ctx)
	if err != nil {
		return domain.SophubRemoteSOP{}, err
	}
	remote, err := service.client.GetSOP(ctx, apiKey, remoteSOPID)
	if err != nil {
		return domain.SophubRemoteSOP{}, fmt.Errorf("Sophub SOP fetch failed")
	}
	if remote.ID != remoteSOPID {
		return domain.SophubRemoteSOP{}, fmt.Errorf("Sophub SOP identity mismatch")
	}
	// 仅公开 approved single-file markdown 可被 Worker 安装(方案 §5.2)。
	if !isSophubSOPPublic(remote) || !strings.EqualFold(remote.Status, domain.SOPStatusApproved) ||
		!strings.EqualFold(remote.PackageType, domain.SOPPackageTypeSingle) ||
		!strings.EqualFold(remote.FileType, domain.SOPFileTypeMarkdown) {
		return domain.SophubRemoteSOP{}, fmt.Errorf("Sophub SOP is not a public approved single-file markdown")
	}
	return remote, nil
}

func (service *sophubService) bindingKey(ctx context.Context) (string, error) {
	binding, err := service.store.GetSophubBinding(ctx)
	if err != nil {
		return "", err
	}
	plain, err := service.cipher.Decrypt(binding.APIKeyCiphertext, binding.APIKeyVersion)
	if err != nil {
		return "", fmt.Errorf("decrypt Sophub API key")
	}
	apiKey := string(plain)
	if err := validateSophubAPIKey(apiKey); err != nil {
		return "", fmt.Errorf("stored Sophub API key is invalid")
	}
	return apiKey, nil
}

func validateSophubAPIKey(apiKey string) error {
	if apiKey == "" || !utf8.ValidString(apiKey) || len([]byte(apiKey)) > maxSophubAPIKeyBytes {
		return fmt.Errorf("Sophub API key is invalid")
	}
	for _, char := range apiKey {
		if unicode.IsControl(char) || unicode.IsSpace(char) {
			return fmt.Errorf("Sophub API key is invalid")
		}
	}
	return nil
}
