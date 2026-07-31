package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	SOPFileTypeMarkdown    = "markdown"
	MaxSOPRemoteIDBytes    = 256
	MaxSOPTitleBytes       = 200
	MaxSOPDescriptionBytes = 2048
	MaxSOPContentBytes     = 64 * 1024
	MaxSOPReviewNoteBytes  = 2048
	MaxLoadedSOPs          = 16
	MaxLoadedSOPBytes      = 256 * 1024
)

var (
	ErrSophubNotConfigured = errors.New("Sophub binding is not configured")
	ErrSOPCandidateState   = errors.New("SOP candidate state does not allow operation")
	ErrSOPLoadLimit        = errors.New("loaded SOP limit reached")
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
	Content     string `json:"content"`
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

type SOPCandidateStatus string

const (
	SOPCandidatePending  SOPCandidateStatus = "pending"
	SOPCandidateApproved SOPCandidateStatus = "approved"
	SOPCandidateRejected SOPCandidateStatus = "rejected"
)

func (status SOPCandidateStatus) IsValid() bool {
	return status == SOPCandidatePending || status == SOPCandidateApproved || status == SOPCandidateRejected
}

type ImportSOPCandidateCommand struct {
	RemoteSOPID string
	Title       string
	Description string
	FileType    string
	Content     string
}

type SOPCandidate struct {
	ID           string
	RemoteSOPID  string
	Title        string
	Description  string
	FileType     string
	Content      string
	SourceDigest string
	Status       SOPCandidateStatus
	ReviewedBy   int64
	ReviewNote   string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ReviewedAt   *time.Time
}

type SOPEntry struct {
	ID              string
	RemoteSOPID     string
	LoadedVersionID string
	LoadedBy        int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LoadedAt        *time.Time
}

type SOPVersion struct {
	ID            string
	EntryID       string
	CandidateID   string
	Version       int
	Title         string
	Description   string
	Content       string
	ContentDigest string
	ApprovedBy    int64
	ApprovedAt    time.Time
}

type SOPRegistryItem struct {
	Version SOPVersion
	Loaded  bool
}

type TaskSOPSnapshot struct {
	TaskID        string
	Ordinal       int
	SOPVersionID  string
	Title         string
	Description   string
	Content       string
	ContentDigest string
	CreatedAt     time.Time
}

func SOPContentDigest(content string) (string, error) {
	if err := validateSOPContent(content); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:]), nil
}

func ValidateImportSOPCandidate(cmd ImportSOPCandidateCommand) error {
	if err := validateBoundedSOPText("remote SOP id", cmd.RemoteSOPID, MaxSOPRemoteIDBytes, false); err != nil {
		return err
	}
	if err := validateBoundedSOPText("SOP title", cmd.Title, MaxSOPTitleBytes, false); err != nil {
		return err
	}
	if err := validateBoundedSOPText("SOP description", cmd.Description, MaxSOPDescriptionBytes, true); err != nil {
		return err
	}
	if cmd.FileType != SOPFileTypeMarkdown {
		return fmt.Errorf("SOP file type must be %q", SOPFileTypeMarkdown)
	}
	return validateSOPContent(cmd.Content)
}

func ValidateSOPReviewNote(note string) error {
	return validateBoundedSOPText("SOP review note", note, MaxSOPReviewNoteBytes, true)
}

func validateSOPContent(content string) error {
	if !utf8.ValidString(content) || strings.TrimSpace(content) == "" {
		return fmt.Errorf("SOP content must be non-empty UTF-8")
	}
	if len([]byte(content)) > MaxSOPContentBytes {
		return fmt.Errorf("SOP content exceeds %d bytes", MaxSOPContentBytes)
	}
	for _, r := range content {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return fmt.Errorf("SOP content contains unsupported control characters")
		}
	}
	return nil
}

func validateBoundedSOPText(field, value string, maxBytes int, allowEmpty bool) error {
	if !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s is invalid", field)
	}
	if !allowEmpty && value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if len([]byte(value)) > maxBytes {
		return fmt.Errorf("%s exceeds %d bytes", field, maxBytes)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains control characters", field)
		}
	}
	return nil
}
