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
	UpsertSOPCandidate(ctx context.Context, input domain.ImportSOPCandidateCommand) (domain.SOPCandidate, error)
	ListSOPCandidates(ctx context.Context, status domain.SOPCandidateStatus) ([]domain.SOPCandidate, error)
	ApproveSOPCandidate(ctx context.Context, candidateID string, adminUserID int64) (domain.SOPVersion, error)
	RejectSOPCandidate(ctx context.Context, candidateID string, adminUserID int64, note string) error
	ListSOPRegistry(ctx context.Context) ([]domain.SOPRegistryItem, error)
	LoadSOPVersion(ctx context.Context, versionID string, adminUserID int64) (domain.SOPEntry, error)
	UnloadSOP(ctx context.Context, entryID string, adminUserID int64) (domain.SOPEntry, error)
	ListLoadedSOPVersions(ctx context.Context) ([]domain.SOPVersion, error)
}

type SophubService interface {
	Bind(ctx context.Context, apiKey string, adminUserID int64) (domain.SophubBindingStatus, error)
	GetBindingStatus(ctx context.Context) (domain.SophubBindingStatus, error)
	Search(ctx context.Context, query string, page, pageSize int) (domain.SophubSearchResult, error)
	ImportCandidate(ctx context.Context, remoteSOPID string) (domain.SOPCandidate, error)
	ListCandidates(ctx context.Context, status domain.SOPCandidateStatus) ([]domain.SOPCandidate, error)
	ApproveCandidate(ctx context.Context, candidateID string, adminUserID int64) (domain.SOPVersion, error)
	RejectCandidate(ctx context.Context, candidateID string, adminUserID int64, note string) error
	ListRegistry(ctx context.Context) ([]domain.SOPRegistryItem, error)
	LoadVersion(ctx context.Context, versionID string, adminUserID int64) (domain.SOPEntry, error)
	Unload(ctx context.Context, entryID string, adminUserID int64) (domain.SOPEntry, error)
	ListLoaded(ctx context.Context) ([]domain.SOPVersion, error)
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
	if !utf8.ValidString(query) || len([]byte(query)) > domain.MaxSOPTitleBytes {
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
	return result, nil
}

func (service *sophubService) ImportCandidate(ctx context.Context, remoteSOPID string) (domain.SOPCandidate, error) {
	apiKey, err := service.bindingKey(ctx)
	if err != nil {
		return domain.SOPCandidate{}, err
	}
	remote, err := service.client.GetSOP(ctx, apiKey, remoteSOPID)
	if err != nil {
		return domain.SOPCandidate{}, fmt.Errorf("Sophub SOP fetch failed")
	}
	if remote.ID != strings.TrimSpace(remoteSOPID) {
		return domain.SOPCandidate{}, fmt.Errorf("Sophub SOP identity mismatch")
	}
	if remote.Status != "approved" || remote.PackageType != "single_file" {
		return domain.SOPCandidate{}, fmt.Errorf("Sophub SOP must be approved single-file content")
	}
	input := domain.ImportSOPCandidateCommand{
		RemoteSOPID: remote.ID,
		Title:       remote.Title,
		Description: remote.Preview,
		FileType:    remote.FileType,
		Content:     remote.Content,
	}
	if err := domain.ValidateImportSOPCandidate(input); err != nil {
		return domain.SOPCandidate{}, err
	}
	return service.store.UpsertSOPCandidate(ctx, input)
}

func (service *sophubService) ListCandidates(ctx context.Context, status domain.SOPCandidateStatus) ([]domain.SOPCandidate, error) {
	if status != "" && !status.IsValid() {
		return nil, fmt.Errorf("invalid SOP candidate status")
	}
	return service.store.ListSOPCandidates(ctx, status)
}

func (service *sophubService) ApproveCandidate(ctx context.Context, candidateID string, adminUserID int64) (domain.SOPVersion, error) {
	return service.store.ApproveSOPCandidate(ctx, candidateID, adminUserID)
}

func (service *sophubService) RejectCandidate(ctx context.Context, candidateID string, adminUserID int64, note string) error {
	return service.store.RejectSOPCandidate(ctx, candidateID, adminUserID, note)
}

func (service *sophubService) ListRegistry(ctx context.Context) ([]domain.SOPRegistryItem, error) {
	return service.store.ListSOPRegistry(ctx)
}

func (service *sophubService) LoadVersion(ctx context.Context, versionID string, adminUserID int64) (domain.SOPEntry, error) {
	return service.store.LoadSOPVersion(ctx, versionID, adminUserID)
}

func (service *sophubService) Unload(ctx context.Context, entryID string, adminUserID int64) (domain.SOPEntry, error) {
	return service.store.UnloadSOP(ctx, entryID, adminUserID)
}

func (service *sophubService) ListLoaded(ctx context.Context) ([]domain.SOPVersion, error) {
	return service.store.ListLoadedSOPVersions(ctx)
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
