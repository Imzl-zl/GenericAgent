// Package domain workspace key derivation (spec §3: 用户身份与串行调度).
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// WorkspaceScope is the top-level scope of a workspace key.
type WorkspaceScope string

const (
	ScopePersonal WorkspaceScope = "personal"
	ScopeTeam     WorkspaceScope = "team"
)

var errInvalidWorkspaceKey = errors.New("invalid workspace key")

// uuidKeyPattern 匹配 Postgres uuid 列的规范字符串形式（PRD §5: team.id 为 UUID）。
var uuidKeyPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidateWorkspaceKey 校验 workspace key 格式（审查 Minor-1 统一入口）：
// personal:<positive-int> 或 team:<uuid|positive-int>（兼容旧整数格式）。
// WorkspaceDirHash/RunnerKeyForWorkspace 与 Sandbox 的 hash 推导共用此校验，
// 消除 domain 严格校验与 sandbox 宽松 hash 之间的两套逻辑。
func ValidateWorkspaceKey(key string) error {
	scope, idText, found := strings.Cut(key, ":")
	if !found || idText == "" {
		return fmt.Errorf("%w: missing or empty ':' separator", errInvalidWorkspaceKey)
	}
	switch WorkspaceScope(scope) {
	case ScopePersonal:
		id, err := strconv.ParseInt(idText, 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("%w: invalid personal id %q", errInvalidWorkspaceKey, idText)
		}
	case ScopeTeam:
		if !uuidKeyPattern.MatchString(idText) {
			id, err := strconv.ParseInt(idText, 10, 64)
			if err != nil || id <= 0 {
				return fmt.Errorf("%w: invalid team id %q", errInvalidWorkspaceKey, idText)
			}
		}
	default:
		return fmt.Errorf("%w: unknown scope %q", errInvalidWorkspaceKey, scope)
	}
	return nil
}

// PersonalWorkspaceKey derives the personal workspace key for a canonical user.
func PersonalWorkspaceKey(canonicalUserID int64) (string, error) {
	if canonicalUserID <= 0 {
		return "", fmt.Errorf("canonical user id must be positive")
	}
	return fmt.Sprintf("%s:%d", ScopePersonal, canonicalUserID), nil
}

// TeamWorkspaceKey derives the shared team workspace key.
func TeamWorkspaceKey(teamID int64) (string, error) {
	if teamID <= 0 {
		return "", fmt.Errorf("team id must be positive")
	}
	return fmt.Sprintf("%s:%d", ScopeTeam, teamID), nil
}

// RunnerKeyForWorkspace returns the serialization key for a workspace.
// Per spec §3 the runner key and workspace key have the same form:
// personal:<canonical_user_id> or team:<team_id>.
func RunnerKeyForWorkspace(workspaceKey string) (string, error) {
	if err := ValidateWorkspaceKey(workspaceKey); err != nil {
		return "", err
	}
	return workspaceKey, nil
}

// ParseWorkspaceKey validates and splits a workspace key into scope and id.
func ParseWorkspaceKey(key string) (WorkspaceScope, int64, error) {
	scope, idText, found := strings.Cut(key, ":")
	if !found {
		return "", 0, fmt.Errorf("%w: missing ':' separator", errInvalidWorkspaceKey)
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 {
		return "", 0, fmt.Errorf("%w: invalid id %q", errInvalidWorkspaceKey, idText)
	}
	switch WorkspaceScope(scope) {
	case ScopePersonal, ScopeTeam:
		return WorkspaceScope(scope), id, nil
	default:
		return "", 0, fmt.Errorf("%w: unknown scope %q", errInvalidWorkspaceKey, scope)
	}
}

// WorkspaceDirHash returns the stable directory hash for a workspace key
// (spec §4: workspaces/<hash(workspace_key)>/).
func WorkspaceDirHash(workspaceKey string) (string, error) {
	if err := ValidateWorkspaceKey(workspaceKey); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(workspaceKey))
	return hex.EncodeToString(sum[:]), nil
}

// RunnerLeaseStatus is the lifecycle state of a workspace Runner lease.
type RunnerLeaseStatus string

const (
	RunnerLeaseActive RunnerLeaseStatus = "active"
)

// RunnerLease is the durable control-plane record for one workspace Runner
// (spec §3). It is a persisted record, not a process-local cache; creation,
// reuse, reclamation and orphan cleanup all fence on RunnerGeneration.
type RunnerLease struct {
	RunnerKey      string
	Owner          string // lease owner (platform instance id)
	Generation     uint64 // monotonically increasing per Runner recreation
	ContainerID    string // immutable once attached
	ControlEndpoint string
	// StaleContainerID 是 lease 接管/重建时被替换的旧容器 ID, 供调用方定向
	// 清理(审查 C6: 接管事务不再丢失旧容器身份)。
	StaleContainerID string
	Status          RunnerLeaseStatus
	ExpiresAt       time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// IsExpired reports whether the lease is past its expiry.
func (l RunnerLease) IsExpired(now time.Time) bool { return now.After(l.ExpiresAt) }
