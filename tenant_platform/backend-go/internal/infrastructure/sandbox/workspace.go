package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WorkspaceDirs holds the resolved workspace subpaths for a Runner.
type WorkspaceDirs struct {
	Workspace   string // workspaces/<hash>
	Memory      string
	Temp        string
	State       string
	Config      string
	Attachments string
}

// 工作区目录/文件权限: 属主固定为 Runner uid:gid(默认 10002:10002)。
// Platform 通过 compose group_add 共享 gid 后同样可读写(方案 §7 共享卷)。
const (
	workspaceDirsMode  = 0o770
	workspaceFilesMode = 0o640
)

// prepareWorkspaceDirs creates workspaces/<hash>/{memory,temp,state,config,
// attachments} under root with fixed non-root ownership, and seeds memory
// from the read-only image template on first creation (empty memory). Existing
// user memory is never overwritten (spec §4: 模板升级不得静默覆盖用户修改).
//
// config/ 与 attachments/ 由控制面(Platform/Manager)写入, Runner 以只读挂载;
// memory/temp/state 由 Runner 读写。chown 仅在 Manager 以 root 运行时执行;
// 非 root 环境(单元测试)跳过, 属主不符会在 Runner 容器启动后以写入失败
// 显式暴露, 不会静默掩盖。
func prepareWorkspaceDirs(root, workspaceHash, memoryTemplate string, uid, gid int) (WorkspaceDirs, error) {
	if !workspaceHashPattern.MatchString(workspaceHash) {
		return WorkspaceDirs{}, fmt.Errorf("invalid workspace hash %q", workspaceHash)
	}

	ws := filepath.Join(root, workspaceHash)
	dirs := WorkspaceDirs{
		Workspace:   ws,
		Memory:      filepath.Join(ws, "memory"),
		Temp:        filepath.Join(ws, "temp"),
		State:       filepath.Join(ws, "state"),
		Config:      filepath.Join(ws, "config"),
		Attachments: filepath.Join(ws, "attachments"),
	}
	for _, d := range []string{ws, dirs.Memory, dirs.Temp, dirs.State, dirs.Config, dirs.Attachments} {
		if err := os.MkdirAll(d, workspaceDirsMode); err != nil {
			return WorkspaceDirs{}, fmt.Errorf("create workspace dir %s: %w", d, err)
		}
	}
	if os.Geteuid() == 0 {
		for _, d := range []string{ws, dirs.Memory, dirs.Temp, dirs.State, dirs.Config, dirs.Attachments} {
			if err := os.Chown(d, uid, gid); err != nil {
				return WorkspaceDirs{}, fmt.Errorf("chown workspace dir %s: %w", d, err)
			}
		}
	}

	// 首次创建(memory 为空)时从镜像内只读模板初始化基线。
	if memoryTemplate != "" {
		entries, err := os.ReadDir(dirs.Memory)
		if err != nil {
			return WorkspaceDirs{}, fmt.Errorf("read memory dir: %w", err)
		}
		if len(entries) == 0 {
			if err := copyTree(memoryTemplate, dirs.Memory, uid, gid); err != nil {
				return WorkspaceDirs{}, fmt.Errorf("seed memory template: %w", err)
			}
		}
	}
	return dirs, nil
}

// copyTree copies src directory contents into dst (dst must exist).
func copyTree(src, dst string, uid, gid int) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := os.MkdirAll(dstPath, workspaceDirsMode); err != nil {
				return err
			}
			if err := copyTree(srcPath, dstPath, uid, gid); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(srcPath)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dstPath, data, workspaceFilesMode); err != nil {
			return err
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(dstPath, uid, gid); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeConfigFiles atomically writes the control-plane files (mTLS material,
// policy manifest) into workspaces/<hash>/config/. File names are restricted
// to plain base names so nothing can escape the config directory.
func writeConfigFiles(root, workspaceHash string, files map[string][]byte, uid, gid int) error {
	if len(files) == 0 {
		return nil
	}
	dir := filepath.Join(root, workspaceHash, "config")
	for name, data := range files {
		if name == "" || filepath.Base(name) != name || strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
			return fmt.Errorf("unsafe config file name %q", name)
		}
		path := filepath.Join(dir, name)
		tmp, err := os.CreateTemp(dir, name+".*.tmp")
		if err != nil {
			return fmt.Errorf("create temp for %s: %w", name, err)
		}
		tmpName := tmp.Name()
		cleanup := true
		defer func() {
			if cleanup {
				_ = os.Remove(tmpName)
			}
		}()
		if _, err := tmp.Write(data); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("write %s: %w", name, err)
		}
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("sync %s: %w", name, err)
		}
		if err := tmp.Close(); err != nil {
			return fmt.Errorf("close %s: %w", name, err)
		}
		if err := os.Chmod(tmpName, 0o600); err != nil {
			return fmt.Errorf("chmod %s: %w", name, err)
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(tmpName, uid, gid); err != nil {
				return fmt.Errorf("chown %s: %w", name, err)
			}
		}
		if err := os.Rename(tmpName, path); err != nil {
			return fmt.Errorf("rename %s: %w", name, err)
		}
		cleanup = false
	}
	return nil
}
