package domain

import (
	"errors"
	"time"
)

const (
	SOPFileTypeMarkdown    = "markdown"
	SOPPackageTypeSingle   = "single_file"
	SOPStatusApproved      = "approved"
	MaxSOPRemoteIDBytes = 256
	MaxSOPContentBytes  = 64 * 1024
)

var (
	ErrSophubNotConfigured = errors.New("Sophub binding is not configured")
)

type SophubIdentity struct {
	AuthorType  string `json:"author_type"`
	AgentUID    string `json:"agent_uid"`
	DisplayName string `json:"display_name"`
}

type SophubRemoteSOP struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Preview     string `json:"preview"`
	FileType    string `json:"file_type"`
	PackageType string `json:"package_type"`
	Status      string `json:"status"`
	// Source 是远程发布源(2026-08 实测 fudankw.cn/sophub 列表与详情均
	// 不返回 is_public 字段): 公开 SOP 以 source=community(社区分享)
	// + status=approved 表达。IsPublic 用指针区分"缺失"与"显式 false"
	// (显式 false 仍拒绝), 见 application/isSophubSOPPublic。
	IsPublic *bool `json:"is_public"`
	Content  string `json:"content"`
	Source   string `json:"source"`
}

type SophubSearchResult struct {
	Items      []SophubRemoteSOP `json:"items"`
	Total      int               `json:"total"`
	Page       int               `json:"page"`
	PageSize   int               `json:"page_size"`
	TotalPages int               `json:"total_pages"`
	HasMore    bool              `json:"has_more"`
}

type SophubBinding struct {
	APIKeyCiphertext []byte
	APIKeyVersion    int
	Identity         SophubIdentity
	VerifiedAt       *time.Time
	UpdatedBy        int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type SophubBindingStatus struct {
	Configured  bool
	AuthorType  string
	AgentUID    string
	DisplayName string
	VerifiedAt  *time.Time
	UpdatedAt   time.Time
}

func (binding SophubBinding) Status() SophubBindingStatus {
	return SophubBindingStatus{
		Configured:  len(binding.APIKeyCiphertext) > 0,
		AuthorType:  binding.Identity.AuthorType,
		AgentUID:    binding.Identity.AgentUID,
		DisplayName: binding.Identity.DisplayName,
		VerifiedAt:  binding.VerifiedAt,
		UpdatedAt:   binding.UpdatedAt,
	}
}
