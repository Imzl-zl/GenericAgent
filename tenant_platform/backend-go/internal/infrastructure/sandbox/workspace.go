package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
)

// WorkspaceDirs holds the resolved workspace subpaths for a Runner.
type WorkspaceDirs struct {
	Workspace string // workspaces/<hash>
	Memory    string
	Temp      string
	State     string
}

// prepareWorkspaceDirs creates workspaces/<hash>/{memory,temp,state} under
// root with the fixed non-root ownership mode, and seeds memory from the
// read-only image template on first creation (empty memory). Existing user
// memory is never overwritten (spec §4: 模板升级不得静默覆盖用户修改).
func prepareWorkspaceDirs(root, workspaceHash, memoryTemplate string) (WorkspaceDirs, error) {
	ok, err := workspaceHashPattern.MatchString(workspaceHash), error(nil)
	if !ok {
		return WorkspaceDirs{}, fmt.Errorf("invalid workspace hash %q", workspaceHash)
	}
	_ = err

	ws := filepath.Join(root, workspaceHash)
	dirs := WorkspaceDirs{
		Workspace: ws,
		Memory:    filepath.Join(ws, "memory"),
		Temp:      filepath.Join(ws, "temp"),
		State:     filepath.Join(ws, "state"),
	}
	for _, d := range []string{dirs.Memory, dirs.Temp, dirs.State} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return WorkspaceDirs{}, fmt.Errorf("create workspace dir %s: %w", d, err)
		}
	}

	// 首次创建(memory 为空)时从镜像内只读模板初始化基线。
	if memoryTemplate != "" {
		entries, err := os.ReadDir(dirs.Memory)
		if err != nil {
			return WorkspaceDirs{}, fmt.Errorf("read memory dir: %w", err)
		}
		if len(entries) == 0 {
			if err := copyTree(memoryTemplate, dirs.Memory); err != nil {
				return WorkspaceDirs{}, fmt.Errorf("seed memory template: %w", err)
			}
		}
	}
	return dirs, nil
}

// copyTree copies src directory contents into dst (dst must exist).
func copyTree(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := os.MkdirAll(dstPath, 0o750); err != nil {
				return err
			}
			if err := copyTree(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(srcPath)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dstPath, data, 0o640); err != nil {
			return err
		}
	}
	return nil
}
