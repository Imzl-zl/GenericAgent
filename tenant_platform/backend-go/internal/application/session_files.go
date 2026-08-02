package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	sessionFilesDirName   = "session_files"
	sessionManifestName   = "manifest.json"
	sessionAttachmentsDir = "attachments"
	sessionOutputsDir     = "outputs"
)

var fileMarkerRE = regexp.MustCompile(`\[FILE:([^\]]+)\]`)

// SessionFileRef is one file visible inside a session sandbox.
type SessionFileRef struct {
	Alias        string    `json:"alias"`
	OriginalName string    `json:"original_name"`
	RelativePath string    `json:"relative_path"`
	Direction    string    `json:"direction"`
	CreatedAt    time.Time `json:"created_at"`
}

// SessionFiles manages per-session file sandboxes for inbound attachments and generated outputs.
type SessionFiles interface {
	ImportInbound(sessionKey string, sourcePaths []string) ([]SessionFileRef, error)
	Recent(sessionKey string, limit int) ([]SessionFileRef, error)
	ResolveMarker(sessionKey, marker string) (absPath string, relPath string, err error)
	RecordOutbound(sessionKey, marker string) (SessionFileRef, error)
	SandboxRoot(sessionKey string) string
}

type sessionFilesManager struct {
	root string
	// workspaceLayout 为 true 时(root = GA_WORKSPACES_ROOT)按
	// <root>/<workspace-hash>/temp/session_files/<digest> 布局落盘:
	// 附件/输出经共享卷 temp 对 Runner 可见(方案 §4/§6)。
	workspaceLayout bool
	mu               sync.Map // session hash -> *sync.Mutex
}

type sessionManifest struct {
	NextSeq int              `json:"next_seq"`
	Files   []SessionFileRef `json:"files"`
}

func NewSessionFiles(root string) (SessionFiles, error) {
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
	return &sessionFilesManager{root: base}, nil
}

// NewWorkspaceSessionFiles 构建共享卷布局的 SessionFiles(生产 Runner 模式):
// 附件/输出落在 workspaces/<hash>/temp/session_files/..., 与 Runner 内
// GA_WORKSPACE_TEMP(/ga/legacy/temp) 的 worker 侧布局完全一致。
func NewWorkspaceSessionFiles(workspacesRoot string) (SessionFiles, error) {
	if strings.TrimSpace(workspacesRoot) == "" {
		return nil, fmt.Errorf("workspaces root is required")
	}
	return &sessionFilesManager{root: filepath.Clean(workspacesRoot), workspaceLayout: true}, nil
}

func (m *sessionFilesManager) SandboxRoot(sessionKey string) string {
	if m.workspaceLayout {
		hash := sessionKeyDigest(sessionKey)
		return filepath.Join(m.root, hash, "temp", sessionFilesDirName, hash)
	}
	return filepath.Join(m.root, sessionKeyDigest(sessionKey))
}

func (m *sessionFilesManager) ImportInbound(sessionKey string, sourcePaths []string) ([]SessionFileRef, error) {
	if len(sourcePaths) == 0 {
		return nil, nil
	}
	lock := m.lockFor(sessionKey)
	lock.Lock()
	defer lock.Unlock()

	root := m.SandboxRoot(sessionKey)
	if err := os.MkdirAll(filepath.Join(root, sessionAttachmentsDir), 0o755); err != nil {
		return nil, fmt.Errorf("create attachments dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, sessionOutputsDir), 0o755); err != nil {
		return nil, fmt.Errorf("create outputs dir: %w", err)
	}
	manifest, err := m.loadManifest(root)
	if err != nil {
		return nil, err
	}
	imported := make([]SessionFileRef, 0, len(sourcePaths))
	for _, src := range sourcePaths {
		if strings.TrimSpace(src) == "" {
			continue
		}
		info, err := os.Stat(src)
		if err != nil {
			return nil, fmt.Errorf("stat source attachment %q: %w", src, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("source attachment %q is a directory", src)
		}
		manifest.NextSeq++
		alias := fmt.Sprintf("F%03d", manifest.NextSeq)
		safeName := sanitizeFileName(filepath.Base(src))
		if safeName == "" {
			safeName = "file"
		}
		rel := filepath.ToSlash(filepath.Join(sessionAttachmentsDir, alias+"_"+safeName))
		dst := filepath.Join(root, filepath.FromSlash(rel))
		if err := copyFile(src, dst); err != nil {
			return nil, fmt.Errorf("copy attachment %q: %w", src, err)
		}
		ref := SessionFileRef{
			Alias:        alias,
			OriginalName: filepath.Base(src),
			RelativePath: rel,
			Direction:    "inbound",
			CreatedAt:    time.Now().UTC(),
		}
		manifest.Files = append(manifest.Files, ref)
		imported = append(imported, ref)
	}
	if err := m.saveManifest(root, manifest); err != nil {
		return nil, err
	}
	return imported, nil
}

func (m *sessionFilesManager) Recent(sessionKey string, limit int) ([]SessionFileRef, error) {
	if limit <= 0 {
		limit = 8
	}
	lock := m.lockFor(sessionKey)
	lock.Lock()
	defer lock.Unlock()
	manifest, err := m.loadManifest(m.SandboxRoot(sessionKey))
	if err != nil {
		return nil, err
	}
	files := append([]SessionFileRef(nil), manifest.Files...)
	sort.Slice(files, func(i, j int) bool {
		return files[i].CreatedAt.After(files[j].CreatedAt)
	})
	if len(files) > limit {
		files = files[:limit]
	}
	return files, nil
}

func (m *sessionFilesManager) ResolveMarker(sessionKey, marker string) (string, string, error) {
	if strings.TrimSpace(marker) == "" {
		return "", "", fmt.Errorf("file marker is empty")
	}
	root := m.SandboxRoot(sessionKey)
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

	root := m.SandboxRoot(sessionKey)
	manifest, err := m.loadManifest(root)
	if err != nil {
		return SessionFileRef{}, err
	}
	resolved, rel, err := resolveUnderRoot(root, marker)
	if err != nil {
		return SessionFileRef{}, err
	}
	for _, item := range manifest.Files {
		if item.RelativePath == rel {
			return item, nil
		}
	}
	if !strings.HasPrefix(rel, sessionOutputsDir+"/") {
		return SessionFileRef{}, fmt.Errorf("outbound file must live under %s", sessionOutputsDir)
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
	b.WriteString("Use file_read/file_write/file_patch only with these relative paths. To create a Word document, use export_docx and save it under outputs/. To send a generated file back to the user, include [FILE:outputs/<filename>] in your final reply.\n")
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

func sessionKeyDigest(sessionKey string) string {
	sum := sha256.Sum256([]byte(sessionKey))
	return hex.EncodeToString(sum[:])
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

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
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
	digest := sessionKeyDigest(sessionKey)
	lock, _ := m.mu.LoadOrStore(digest, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (m *sessionFilesManager) loadManifest(root string) (sessionManifest, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return sessionManifest{}, fmt.Errorf("create session sandbox: %w", err)
	}
	path := filepath.Join(root, sessionManifestName)
	raw, err := os.ReadFile(path)
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
	return manifest, nil
}

func (m *sessionFilesManager) saveManifest(root string, manifest sessionManifest) error {
	path := filepath.Join(root, sessionManifestName)
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session manifest: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write session manifest tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("commit session manifest: %w", err)
	}
	return nil
}
