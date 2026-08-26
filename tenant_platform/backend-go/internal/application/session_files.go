package application

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/safefs"
)

const (
	sessionFilesDirName   = "session_files"
	sessionManifestName   = "manifest.json"
	sessionAttachmentsDir = "attachments"
	sessionOutputsDir     = "outputs"
	// sessionManifestMaxBytes 限制 manifest.json 大小: 文件位于 Runner 可写的
	// 工作区 temp/ 内, 无界读取会让单个租户耗尽 Platform 内存(审查 R4-C4)。
	sessionManifestMaxBytes = 1 << 20 // 1 MiB
	// sessionManifestMaxFiles 限制 manifest 条目数, 防止解析后处理爆炸。
	sessionManifestMaxFiles = 4096
	// sessionManifestNameMaxBytes 限制单条目名称长度(审查 C1): 名称流入
	// 交付快照文件名与用户可见 displayName, 超长名称必须拒绝。
	sessionManifestNameMaxBytes = 255
)

var fileMarkerRE = regexp.MustCompile(`\[FILE:([^\]]+)\]`)

// SessionFileRef is one file visible inside a session sandbox.
type SessionFileRef struct {
	Alias        string    `json:"alias"`
	OriginalName string    `json:"original_name"`
	RelativePath string    `json:"relative_path"`
	Direction    string    `json:"direction"`
	// SizeBytes 是文件字节数(2026-08-13 多模态链路: 随任务媒体清单持久化,
	// GA 据此做注入体积上限判断)。
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// ContentType 是渠道侧/下载器推断的 MIME(2026-08-13 审查 D5): 路由时
	// 从 webhook media_items 按序补入, 随任务媒体清单持久化/下发; 可空
	// (回退扩展名推断)。
	ContentType string    `json:"content_type,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// SessionFiles manages per-session file sandboxes for inbound attachments and generated outputs.
type SessionFiles interface {
	ImportInbound(sessionKey string, sourcePaths []string) ([]SessionFileRef, error)
	// RemoveInbound 回滚一次 ImportInbound(round11 审查 I1): 附件在任务
	// 授权/幂等事务之前写入团队 workspace, 提交失败(成员被移除/重复消息/
	// DB 错误)时必须删除本次导入的文件并从 manifest 移除, 防止未授权
	// 附件残留。幂等: 文件/条目不存在视为成功。
	RemoveInbound(sessionKey string, refs []SessionFileRef) error
	Recent(sessionKey string, limit int) ([]SessionFileRef, error)
	// RecentSince 返回会话最近文件(created_at 降序), 只保留 since 之后创建的
	// (零值 = 不过滤)。会话隔离基础: /new 后旧会话文件不再注入 prompt
	// (2026-08-15 生产实证: 旧附件注入导致新会话任务误转旧文件)。
	RecentSince(sessionKey string, limit int, since time.Time) ([]SessionFileRef, error)
	// PruneInboundBefore 物理删除 before 之前导入的 inbound 附件并从
	// manifest 移除(/new 清理旧会话输入; outbound 产物保留磁盘, 仅引用
	// 隔离)。删除失败不阻塞: 残留文件靠 RecentSince 过滤不可见。
	PruneInboundBefore(sessionKey string, before time.Time) error
	ResolveMarker(sessionKey, marker string) (absPath string, relPath string, err error)
	RecordOutbound(sessionKey, marker string) (SessionFileRef, error)
	// SandboxRoot 返回会话沙箱根路径(workspace 布局: workspaces/<hash>/temp)。
	// hash 推导与容器挂载共用 domain.WorkspaceDirHash 唯一实现(审查 B1 收敛),
	// sessionKey 非法时返回错误——调用方必须显式处理, 不得静默落盘垃圾目录。
	SandboxRoot(sessionKey string) (string, error)
}

type sessionFilesManager struct {
	root string
	// mediaRoot 是入站媒体源根(BotMediaRoot, Poller 下载目录)。非空时
	// ImportInbound 用 CopyFileFromBeneath 以 openat2 原子校验源路径
	// (round9 审查: 消除 EvalSymlinks 预检与复制之间的 TOCTOU 窗口)。
	mediaRoot string
	// workspaceLayout 为 true 时(root = GA_WORKSPACES_ROOT)按
	// <root>/<workspace-hash>/temp/session_files/<digest> 布局落盘:
	// 附件/输出经共享卷 temp 对 Runner 可见(方案 §4/§6)。
	workspaceLayout bool
	// ensureWorkspace 在 ImportInbound 前调用(生产: Manager 控制面预置目录)。
	ensureWorkspace func(sessionKey string) error
	mu               sync.Map // session hash -> *sync.Mutex
}

type sessionManifest struct {
	NextSeq int              `json:"next_seq"`
	Files   []SessionFileRef `json:"files"`
}

func cleanOptionalRoot(root string) string {
	// filepath.Clean("") 返回 ".", 会破坏"空 = 未配置"的语义(round9)。
	if strings.TrimSpace(root) == "" {
		return ""
	}
	return filepath.Clean(root)
}

func NewSessionFiles(root, mediaRoot string) (SessionFiles, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("session files root is required")
	}
	// 方案 §6: 附件/输出统一到工作区 temp; GA_WORKSPACE_TEMP 时以工作区为准。
	if ws := strings.TrimSpace(os.Getenv("GA_WORKSPACE_TEMP")); ws != "" {
		root = ws
	}
	base := filepath.Join(root, sessionFilesDirName)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, fmt.Errorf("create session files root: %w", err)
	}
	return &sessionFilesManager{root: base, mediaRoot: cleanOptionalRoot(mediaRoot)}, nil
}

// NewWorkspaceSessionFiles 构建共享卷布局的 SessionFiles(生产 Runner 模式):
// 附件/输出落在 workspaces/<hash>/temp/session_files/..., 与 Runner 内
// GA_WORKSPACE_TEMP(/ga/legacy/temp) 的 worker 侧布局完全一致。
// ensureWorkspace 非 nil 时在每次附件导入前调用(方案 §6: fresh workspace
// 首条带附件消息必须先由 Manager 预置目录布局与共享组权限)。
func NewWorkspaceSessionFiles(workspacesRoot, mediaRoot string, ensureWorkspace func(sessionKey string) error) (SessionFiles, error) {
	if strings.TrimSpace(workspacesRoot) == "" {
		return nil, fmt.Errorf("workspaces root is required")
	}
	return &sessionFilesManager{
		root:            filepath.Clean(workspacesRoot),
		mediaRoot:       cleanOptionalRoot(mediaRoot),
		workspaceLayout: true,
		ensureWorkspace: ensureWorkspace,
	}, nil
}

func (m *sessionFilesManager) SandboxRoot(sessionKey string) (string, error) {
	hash, err := domain.WorkspaceDirHash(sessionKey)
	if err != nil {
		return "", err
	}
	if m.workspaceLayout {
		// 审查: 附件/输出统一到工作区 temp/ 根(方案 §6), 与 GA 原生 cwd
		// 语义一致; 不再使用 session_files/<digest> 中间层。
		return filepath.Join(m.root, hash, "temp"), nil
	}
	return filepath.Join(m.root, hash), nil
}

// maxInboundMediaBytes 限制入站附件复制上限(对齐 Bot Poller 下载器的
// MAX_MEDIA_BYTES=100MB; Round8 审查: 纵深防御, 防被攻破的 Poller 提交
// 超限文件耗尽 Platform 磁盘)。
const maxInboundMediaBytes = 100 << 20

func (m *sessionFilesManager) ImportInbound(sessionKey string, sourcePaths []string) ([]SessionFileRef, error) {
	if len(sourcePaths) == 0 {
		return nil, nil
	}
	lock := m.lockFor(sessionKey)
	lock.Lock()
	defer lock.Unlock()

	// fresh workspace 首条带附件消息: 先由 Manager 预置目录布局(方案 §6)。
	if m.ensureWorkspace != nil {
		if err := m.ensureWorkspace(sessionKey); err != nil {
			return nil, fmt.Errorf("ensure workspace layout: %w", err)
		}
	}

	root, err := m.SandboxRoot(sessionKey)
	if err != nil {
		return nil, err
	}
	dirMode := m.dirMode()
	// Round8(发现): MkdirAllBeneath 要求 root 已存在(unix.Open O_DIRECTORY);
	// ensureWorkspace 为 nil 的路径(dev loopback)必须先创建 root, 否则
	// Linux 上带附件消息在创建 attachments 目录时失败。
	if err := os.MkdirAll(root, dirMode); err != nil {
		return nil, fmt.Errorf("create session sandbox root: %w", err)
	}
	if err := safefs.MkdirAllBeneath(root, sessionAttachmentsDir, dirMode); err != nil {
		return nil, fmt.Errorf("create attachments dir: %w", err)
	}
	if err := safefs.MkdirAllBeneath(root, sessionOutputsDir, dirMode); err != nil {
		return nil, fmt.Errorf("create outputs dir: %w", err)
	}
	manifest, err := m.loadManifest(root)
	if err != nil {
		return nil, err
	}
	imported := make([]SessionFileRef, 0, len(sourcePaths))
	// round12 审查(I5): 导入必须原子——后续文件复制失败或 manifest 保存失败
	// 时, 删除本次已复制的文件(manifest 最后才保存, 磁盘 manifest 无需恢复),
	// 防止无 manifest 归属的附件残留团队工作区。
	rollbackImported := func() {
		for _, ref := range imported {
			if ref.RelativePath == "" {
				continue
			}
			if err := safefs.RemoveBeneath(root, filepath.FromSlash(ref.RelativePath)); err != nil && !os.IsNotExist(err) {
				slog.WarnContext(context.Background(), "session files: rollback imported attachment failed",
					"session_key", sessionKey, "rel", ref.RelativePath, "error", err)
			}
		}
	}
	for _, src := range sourcePaths {
		if strings.TrimSpace(src) == "" {
			continue
		}
		manifest.NextSeq++
		alias := fmt.Sprintf("F%03d", manifest.NextSeq)
		safeName := sanitizeFileName(filepath.Base(src))
		if safeName == "" {
			safeName = "file"
		}
		rel := filepath.ToSlash(filepath.Join(sessionAttachmentsDir, alias+"_"+safeName))
		if m.mediaRoot != "" {
			// round9 审查: 源路径在 BotMediaRoot 内, 由 openat2 单次解析
			// 完成"不逃逸根 + 无符号链接"校验, 消除预检-复制 TOCTOU 窗口。
			srcRel, err := filepath.Rel(m.mediaRoot, src)
			if err != nil || srcRel == ".." || strings.HasPrefix(srcRel, ".."+string(filepath.Separator)) || filepath.IsAbs(srcRel) {
				rollbackImported()
				return nil, fmt.Errorf("source attachment %q escapes media root %q", src, m.mediaRoot)
			}
			if err := safefs.CopyFileFromBeneath(m.mediaRoot, srcRel, root, filepath.FromSlash(rel), m.fileMode(), maxInboundMediaBytes); err != nil {
				rollbackImported()
				return nil, fmt.Errorf("copy attachment %q: %w", src, err)
			}
		} else {
			// 无媒体根(loopback/dev): 保持旧语义(路径式源, 调用方已校验)。
			info, err := os.Stat(src)
			if err != nil {
				rollbackImported()
				return nil, fmt.Errorf("stat source attachment %q: %w", src, err)
			}
			if info.IsDir() {
				rollbackImported()
				return nil, fmt.Errorf("source attachment %q is a directory", src)
			}
			if err := safefs.CopyFileBeneath(root, filepath.FromSlash(rel), src, m.fileMode(), maxInboundMediaBytes); err != nil {
				rollbackImported()
				return nil, fmt.Errorf("copy attachment %q: %w", src, err)
			}
		}
		ref := SessionFileRef{
			Alias:        alias,
			OriginalName: filepath.Base(src),
			RelativePath: rel,
			Direction:    "inbound",
			CreatedAt:    time.Now().UTC(),
		}
		// 复制完成后 stat 目标拿真实字节数(复制带 maxInboundMediaBytes 上限,
		// 以落盘结果为准)。
		if info, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); statErr == nil {
			ref.SizeBytes = info.Size()
		}
		manifest.Files = append(manifest.Files, ref)
		imported = append(imported, ref)
	}
	if err := m.saveManifest(root, manifest); err != nil {
		rollbackImported()
		return nil, err
	}
	return imported, nil
}

// RemoveInbound 回滚一次导入(round11 审查 I1): 删除 refs 指向的附件文件
// 并从 manifest 移除对应条目。只删除本次导入产生的文件——按 RelativePath
// 精确匹配, 不触碰其他消息导入的附件。幂等: 文件或条目不存在视为成功。
func (m *sessionFilesManager) RemoveInbound(sessionKey string, refs []SessionFileRef) error {
	if len(refs) == 0 {
		return nil
	}
	lock := m.lockFor(sessionKey)
	lock.Lock()
	defer lock.Unlock()

	root, err := m.SandboxRoot(sessionKey)
	if err != nil {
		return fmt.Errorf("rollback inbound: %w", err)
	}
	manifest, err := m.loadManifest(root)
	if err != nil {
		return fmt.Errorf("rollback inbound: load manifest: %w", err)
	}
	removed := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref.RelativePath == "" {
			continue
		}
		rel := filepath.FromSlash(ref.RelativePath)
		if err := safefs.RemoveBeneath(root, rel); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rollback inbound: remove %q: %w", ref.RelativePath, err)
		}
		removed[ref.RelativePath] = struct{}{}
	}
	if len(removed) == 0 {
		return nil
	}
	kept := manifest.Files[:0]
	for _, e := range manifest.Files {
		if _, drop := removed[e.RelativePath]; drop {
			continue
		}
		kept = append(kept, e)
	}
	manifest.Files = kept
	return m.saveManifest(root, manifest)
}

func (m *sessionFilesManager) Recent(sessionKey string, limit int) ([]SessionFileRef, error) {
	return m.RecentSince(sessionKey, limit, time.Time{})
}

func (m *sessionFilesManager) RecentSince(sessionKey string, limit int, since time.Time) ([]SessionFileRef, error) {
	if limit <= 0 {
		limit = 8
	}
	lock := m.lockFor(sessionKey)
	lock.Lock()
	defer lock.Unlock()
	root, err := m.SandboxRoot(sessionKey)
	if err != nil {
		return nil, err
	}
	manifest, err := m.loadManifest(root)
	if err != nil {
		return nil, err
	}
	files := make([]SessionFileRef, 0, len(manifest.Files))
	for _, ref := range manifest.Files {
		if !since.IsZero() && ref.CreatedAt.Before(since) {
			continue
		}
		files = append(files, ref)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].CreatedAt.After(files[j].CreatedAt)
	})
	if len(files) > limit {
		files = files[:limit]
	}
	return files, nil
}

// PruneInboundBefore 删除 before 之前导入的 inbound 附件(2026-08-15 会话
// 隔离: /new 后旧会话输入物理清理, 与新会话文件引用隔离一致)。outbound
// 产物保留磁盘(工作区产物), 引用层由 RecentSince 过滤。文件删除失败仅
// 告警不失败——残留文件不会被注入(引用隔离已保证)。
func (m *sessionFilesManager) PruneInboundBefore(sessionKey string, before time.Time) error {
	if before.IsZero() {
		return nil
	}
	lock := m.lockFor(sessionKey)
	lock.Lock()
	defer lock.Unlock()
	root, err := m.SandboxRoot(sessionKey)
	if err != nil {
		return err
	}
	manifest, err := m.loadManifest(root)
	if err != nil {
		return err
	}
	kept := make([]SessionFileRef, 0, len(manifest.Files))
	pruned := 0
	for _, ref := range manifest.Files {
		if ref.Direction == "inbound" && ref.CreatedAt.Before(before) && ref.RelativePath != "" {
			if err := safefs.RemoveBeneath(root, filepath.FromSlash(ref.RelativePath)); err != nil && !os.IsNotExist(err) {
				slog.WarnContext(context.Background(), "session files: prune inbound attachment failed",
					"session_key", sessionKey, "rel", ref.RelativePath, "error", err)
			}
			pruned++
			continue
		}
		kept = append(kept, ref)
	}
	if pruned == 0 {
		return nil
	}
	manifest.Files = kept
	return m.saveManifest(root, manifest)
}

func (m *sessionFilesManager) ResolveMarker(sessionKey, marker string) (string, string, error) {
	if strings.TrimSpace(marker) == "" {
		return "", "", fmt.Errorf("file marker is empty")
	}
	root, err := m.SandboxRoot(sessionKey)
	if err != nil {
		return "", "", err
	}
	resolved, rel, err := resolveUnderRoot(root, marker)
	if err != nil {
		return "", "", err
	}
	// 安全交付: Lstat 不跟随符号链接, 拒绝非普通文件(方案 §4/§6)。
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", "", fmt.Errorf("resolve file marker %q: %w", marker, err)
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("file marker %q points to a directory", marker)
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("file marker %q is not a regular file (mode %s)", marker, info.Mode())
	}
	return resolved, rel, nil
}

func (m *sessionFilesManager) RecordOutbound(sessionKey, marker string) (SessionFileRef, error) {
	lock := m.lockFor(sessionKey)
	lock.Lock()
	defer lock.Unlock()

	root, err := m.SandboxRoot(sessionKey)
	if err != nil {
		return SessionFileRef{}, err
	}
	resolved, rel, err := resolveUnderRoot(root, marker)
	if err != nil {
		return SessionFileRef{}, err
	}
	// 安全交付: 只有 outputs/ 下的文件才能登记为出站交付物。目录校验必须先于
	// manifest 查询, 否则 Runner 可伪造 manifest 条目绕过 outputs/ 限制
	// (审查 R4-I7: 直接返回附件或 temp/ 下任意文件)。
	if !strings.HasPrefix(rel, sessionOutputsDir+"/") {
		return SessionFileRef{}, fmt.Errorf("outbound file must live under %s", sessionOutputsDir)
	}
	manifest, err := m.loadManifest(root)
	if err != nil {
		return SessionFileRef{}, err
	}
	for _, item := range manifest.Files {
		if item.RelativePath == rel {
			return item, nil
		}
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return SessionFileRef{}, fmt.Errorf("stat outbound file %q: %w", resolved, err)
	}
	if info.IsDir() {
		return SessionFileRef{}, fmt.Errorf("outbound path %q is a directory", resolved)
	}
	if !info.Mode().IsRegular() {
		return SessionFileRef{}, fmt.Errorf("outbound path %q is not a regular file (mode %s)", resolved, info.Mode())
	}
	manifest.NextSeq++
	ref := SessionFileRef{
		Alias:        fmt.Sprintf("F%03d", manifest.NextSeq),
		OriginalName: filepath.Base(resolved),
		RelativePath: rel,
		Direction:    "outbound",
		CreatedAt:    time.Now().UTC(),
	}
	manifest.Files = append(manifest.Files, ref)
	if err := m.saveManifest(root, manifest); err != nil {
		return SessionFileRef{}, err
	}
	return ref, nil
}

func extractFileMarkers(text string) []string {
	matches := fileMarkerRE.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		marker := strings.TrimSpace(match[1])
		if marker == "" {
			continue
		}
		if _, ok := seen[marker]; ok {
			continue
		}
		seen[marker] = struct{}{}
		out = append(out, marker)
	}
	return out
}

func stripFileMarkers(text string) string {
	cleaned := fileMarkerRE.ReplaceAllString(text, "")
	cleaned = strings.ReplaceAll(cleaned, "\r\n", "\n")
	cleaned = regexp.MustCompile(`\n{3,}`).ReplaceAllString(cleaned, "\n\n")
	return strings.TrimSpace(cleaned)
}

func sessionFilesPrompt(current []SessionFileRef, recent []SessionFileRef) string {
	if len(current) == 0 && len(recent) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[Session file workspace]\n")
	b.WriteString("Use file_read/file_write/file_patch only with these relative paths. To create a Word document, call export_docx (it converts markdown/html via the built-in pandoc with Chinese font styling) and save it under outputs/. To send a generated file back to the user, include [FILE:outputs/<filename>] in your final reply.\n")
	if len(current) > 0 {
		b.WriteString("Current attachments:\n")
		for _, ref := range current {
			fmt.Fprintf(&b, "- %s %s => %s\n", ref.Alias, ref.OriginalName, ref.RelativePath)
		}
	}
	if len(recent) > 0 {
		b.WriteString("Recent session files (newest first):\n")
		for _, ref := range recent {
			fmt.Fprintf(&b, "- %s [%s] %s => %s\n", ref.Alias, ref.Direction, ref.OriginalName, ref.RelativePath)
		}
	}
	return strings.TrimSpace(b.String())
}

func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, `\\`, "_")
	name = strings.ReplaceAll(name, ":", "_")
	name = strings.ReplaceAll(name, "*", "_")
	name = strings.ReplaceAll(name, "?", "_")
	name = strings.ReplaceAll(name, `"`, "_")
	name = strings.ReplaceAll(name, "<", "_")
	name = strings.ReplaceAll(name, ">", "_")
	name = strings.ReplaceAll(name, "|", "_")
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}

// dirMode 返回会话目录模式: workspace 共享卷布局用 0770(Runner 属主可写,
// 方案 §6), 其他布局保持 0755。
func (m *sessionFilesManager) dirMode() os.FileMode {
	if m.workspaceLayout {
		return 0o770
	}
	return 0o755
}

// fileMode 返回会话文件模式: workspace 布局 0640(共享组可读), 其他 0644。
func (m *sessionFilesManager) fileMode() os.FileMode {
	if m.workspaceLayout {
		return 0o640
	}
	return 0o644
}

func resolveUnderRoot(root, marker string) (string, string, error) {
	base, err := filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve sandbox root: %w", err)
	}
	var candidate string
	if filepath.IsAbs(marker) {
		candidate = marker
	} else {
		candidate = filepath.Join(base, filepath.FromSlash(marker))
	}
	resolved, err := filepath.Abs(candidate)
	if err != nil {
		return "", "", fmt.Errorf("resolve candidate path: %w", err)
	}
	rel, err := filepath.Rel(base, resolved)
	if err != nil {
		return "", "", fmt.Errorf("resolve relative path: %w", err)
	}
	rel = filepath.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path escapes session sandbox: %s", marker)
	}
	// 中间组件符号链接也可能逃出沙箱(Lstat 只检查最后组件): EvalSymlinks
	// 解析全部组件后必须仍落在 base 内(方案 §4: 不跟随链接)。
	evaluated, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", "", fmt.Errorf("resolve symlinks for %q: %w", marker, err)
	}
	evalRel, err := filepath.Rel(base, evaluated)
	if err != nil {
		return "", "", fmt.Errorf("resolve evaluated path: %w", err)
	}
	evalRel = filepath.Clean(evalRel)
	if evalRel == ".." || strings.HasPrefix(evalRel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path escapes session sandbox via symlink: %s", marker)
	}
	return resolved, filepath.ToSlash(rel), nil
}

func (m *sessionFilesManager) lockFor(sessionKey string) *sync.Mutex {
	lock, _ := m.mu.LoadOrStore(sessionKey, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (m *sessionFilesManager) loadManifest(root string) (sessionManifest, error) {
	if err := os.MkdirAll(root, m.dirMode()); err != nil {
		return sessionManifest{}, fmt.Errorf("create session sandbox: %w", err)
	}
	raw, err := safefs.ReadFileBeneathLimited(root, sessionManifestName, sessionManifestMaxBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return sessionManifest{}, nil
		}
		return sessionManifest{}, fmt.Errorf("read session manifest: %w", err)
	}
	var manifest sessionManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return sessionManifest{}, fmt.Errorf("decode session manifest: %w", err)
	}
	if len(manifest.Files) > sessionManifestMaxFiles {
		return sessionManifest{}, fmt.Errorf("session manifest has %d files (max %d)", len(manifest.Files), sessionManifestMaxFiles)
	}
	if manifest.NextSeq < 0 || manifest.NextSeq > sessionManifestMaxFiles*2 {
		return sessionManifest{}, fmt.Errorf("session manifest next_seq %d out of range", manifest.NextSeq)
	}
	// 审查 C1: manifest 位于 Runner 可写的 temp/ 下, 条目字段不可信——
	// OriginalName 会流入交付快照文件名与用户可见 displayName, RelativePath
	// 会流入提示上下文。非法条目 fail-closed, 不得静默丢弃(否则攻击者可
	// 通过丢弃合法条目制造混乱)。
	for _, e := range manifest.Files {
		if err := validateManifestEntry(e); err != nil {
			return sessionManifest{}, err
		}
	}
	return manifest, nil
}

// validateManifestEntry 校验 manifest 单个条目(审查 C1)。
func validateManifestEntry(e SessionFileRef) error {
	if e.Alias == "" || strings.HasPrefix(e.Alias, "..") || strings.ContainsAny(e.Alias, `\/`) {
		return fmt.Errorf("session manifest entry has unsafe alias %q", e.Alias)
	}
	if e.OriginalName == "" || e.OriginalName == "." || e.OriginalName == ".." ||
		filepath.Base(e.OriginalName) != e.OriginalName || strings.ContainsAny(e.OriginalName, `\/`) ||
		len(e.OriginalName) > sessionManifestNameMaxBytes {
		return fmt.Errorf("session manifest entry %q has unsafe original_name %q", e.Alias, e.OriginalName)
	}
	rel := filepath.FromSlash(e.RelativePath)
	cleaned := filepath.Clean(rel)
	if e.RelativePath == "" || filepath.IsAbs(rel) || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) || strings.Contains(rel, "\x00") ||
		cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		// 审查 C1 收紧: Clean 后仍含 .. 段(outputs/../../x)
		// 等中段跑逃一律拒绝。
		return fmt.Errorf("session manifest entry %q relative path escapes: %q", e.Alias, e.RelativePath)
	}
	return nil
}

func (m *sessionFilesManager) saveManifest(root string, manifest sessionManifest) error {
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session manifest: %w", err)
	}
	if err := safefs.AtomicWriteBeneath(root, sessionManifestName, raw, m.fileMode()); err != nil {
		return fmt.Errorf("commit session manifest: %w", err)
	}
	return nil
}
