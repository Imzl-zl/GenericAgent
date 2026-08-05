package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WorkspaceDirs holds the resolved workspace subpaths for a Runner.
type WorkspaceDirs struct {
	Workspace string // workspaces/<hash>
	Memory    string
	Temp      string
	State     string
	Config    string
}

// 工作区目录/文件权限: 目录属主固定为 Runner uid, 属组固定为共享组
// ShareGID(默认 10003, compose group_add 与 Platform 共享); 目录带 setgid,
// 使 Runner 新建文件继承共享组, Platform 才能读回交付文件(方案 §7 共享卷)。
const (
	workspaceDirsMode  = 0o2770
	// memoryInitMarkerName 标记 memory 已完成首次模板初始化(round11 审查
	// I7): 空目录不再是"首次初始化"的判据——用户主动清空 memory 后不得
	// 重新灌入模板。标记位于工作区根, 不在 Runner 挂载(memory/temp/state)
	// 内, Runner/用户无法删除。
	memoryInitMarkerName = ".ga-memory-init"
	workspaceFilesMode = 0o640
)

// ensureDirPathNoSymlink 从 root 开始逐组件检查 path 的每个已存在组件:
// 符号链接直接删除(只 unlink 链接本身, 不触碰链接目标), 非目录组件报错;
// 之后调用方用 MkdirAll 重建为目录。防止 Runner(共享组)用 symlink 替换目录
// 后 Manager(root) 的 MkdirAll/Chown/Chmod 跟随链接修改工作区外目标
// (审查: 跨工作区越权/宿主路径属性篡改)。root 及以上(部署路径/卷挂载点)
// 视为受信边界, 不扫描也不允许删除(审查 review I1: 不得破坏部署 symlink)。
func ensureDirPathNoSymlink(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("path %s escapes workspace root %s", path, root)
	}
	cur := root
	for _, part := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("lstat %s: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(cur); err != nil {
				return fmt.Errorf("remove symlink %s: %w", cur, err)
			}
			continue
		}
		if !info.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", cur)
		}
	}
	return nil
}

// prepareWorkspaceDirs creates workspaces/<hash>/{memory,temp,state,config}
// under root with fixed non-root ownership, and seeds memory from the read-only
// image template on first creation (empty memory). Existing user memory is
// never overwritten (spec §4: 模板升级不得静默覆盖用户修改).
//
// config/ 由控制面(Platform/Manager)写入, Runner 以只读挂载; memory/temp/state
// 由 Runner 读写。同时预置 temp/attachments 与 temp/outputs 子目录(方案 §6:
// 附件/输出经共享卷 temp 互通, 与 GA 原生 cwd 语义一致)。
// chown 仅在 Manager 以 root 运行时执行; 非 root 环境(单元测试)跳过, 属主不符
// 会在 Runner 容器启动后以写入失败显式暴露, 不会静默掩盖。
func prepareWorkspaceDirs(root, workspaceHash, memoryTemplate string, uid, gid, shareGID int) (WorkspaceDirs, error) {
	if !workspaceHashPattern.MatchString(workspaceHash) {
		return WorkspaceDirs{}, fmt.Errorf("invalid workspace hash %q", workspaceHash)
	}
	if shareGID <= 0 {
		shareGID = gid
	}

	ws := filepath.Join(root, workspaceHash)
	dirs := WorkspaceDirs{
		Workspace: ws,
		Memory:    filepath.Join(ws, "memory"),
		Temp:      filepath.Join(ws, "temp"),
		State:     filepath.Join(ws, "state"),
		Config:    filepath.Join(ws, "config"),
	}
	dirsToCreate := []string{
		ws, dirs.Memory, dirs.Temp, dirs.State, dirs.Config,
		// checkpoint 目录由 Manager 以共享组预置(2770): Platform 运行时
		// 的 MkdirAllBeneath 对已存在目录不改权限, 否则 Platform(umask
		// 0027)创建的是 0750, Runner 无法写入 staging(审查: 共享卷权限)。
		filepath.Join(dirs.State, "staging"),
		filepath.Join(dirs.State, "committed"),
		filepath.Join(dirs.State, "results"),
		filepath.Join(dirs.Temp, "attachments"),
		filepath.Join(dirs.Temp, "outputs"),
	}
	// 权限链闭环(审查): 中间目录(如 temp/attachments 的父链)也必须
	// chown/chmod 到 Runner uid + 共享组, 否则 Runner 无法穿过任意一级。
	// 收集每个目标的全部祖先(从 ws 开始), 去重后统一处理。
	dirsSet := map[string]struct{}{}
	for _, d := range dirsToCreate {
		rel, err := filepath.Rel(ws, d)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return WorkspaceDirs{}, fmt.Errorf("workspace dir %s escapes %s", d, ws)
		}
		parent := ws
		dirsSet[parent] = struct{}{}
		for _, part := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
			if part == "" || part == "." {
				continue
			}
			parent = filepath.Join(parent, part)
			dirsSet[parent] = struct{}{}
		}
	}
	allDirs := make([]string, 0, len(dirsSet))
	for d := range dirsSet {
		allDirs = append(allDirs, d)
	}
	sort.Strings(allDirs)

	// 目录创建 + 属主修复全程基于 dirfd(openat O_NOFOLLOW + fchown/fchmod),
	// 不跟随符号链接(审查): Runner(共享组)可在检查后把目录替换为指向宿主
	// 路径的 symlink, 路径式 Chown/Chmod 会跟随链接越权修改工作区外目标。
	// unix 实现见 workspace_unix.go; 非 unix 平台退化为带 Lstat 复检的路径式。
	if err := ensureWorkspaceDirsBeneath(root, allDirs, uid, shareGID); err != nil {
		return WorkspaceDirs{}, err
	}

	// 首次初始化判定(round11 审查 I7): 使用 Runner 不可见的初始化标记,
	// 而非 memory 目录是否为空——用户主动清空 memory 后不得重新灌入模板。
	if memoryTemplate != "" {
		markerPath := filepath.Join(ws, memoryInitMarkerName)
		_, markerErr := os.Stat(markerPath)
		markerExists := markerErr == nil
		if markerErr != nil && !os.IsNotExist(markerErr) {
			return WorkspaceDirs{}, fmt.Errorf("stat memory init marker: %w", markerErr)
		}
		entries, err := os.ReadDir(dirs.Memory)
		if err != nil {
			return WorkspaceDirs{}, fmt.Errorf("read memory dir: %w", err)
		}
		if len(entries) == 0 && !markerExists {
			// 原子初始化(审查): 先复制到同文件系统临时目录再整体 rename。
			// 复制中途失败/崩溃不会留下"目录非空但内容残缺"的半成品基线,
			// 否则下次因目录非空直接跳过初始化, 用户得到不完整模板。
			tmpDir, err := os.MkdirTemp(ws, ".memory-init-*")
			if err != nil {
				return WorkspaceDirs{}, fmt.Errorf("create memory init staging: %w", err)
			}
			if err := copyTree(memoryTemplate, tmpDir, uid, gid, shareGID); err != nil {
				_ = os.RemoveAll(tmpDir)
				return WorkspaceDirs{}, fmt.Errorf("seed memory template: %w", err)
			}
			if os.Geteuid() == 0 {
				if err := os.Chown(tmpDir, uid, shareGID); err != nil {
					_ = os.RemoveAll(tmpDir)
					return WorkspaceDirs{}, fmt.Errorf("chown memory staging: %w", err)
				}
				// chown 会清除 setgid, 需在 chown 之后重新设置。
				if err := os.Chmod(tmpDir, workspaceDirsMode); err != nil {
					_ = os.RemoveAll(tmpDir)
					return WorkspaceDirs{}, fmt.Errorf("chmod memory staging: %w", err)
				}
			}
			if err := os.RemoveAll(dirs.Memory); err != nil {
				_ = os.RemoveAll(tmpDir)
				return WorkspaceDirs{}, fmt.Errorf("remove empty memory dir: %w", err)
			}
			if err := os.Rename(tmpDir, dirs.Memory); err != nil {
				_ = os.RemoveAll(tmpDir)
				return WorkspaceDirs{}, fmt.Errorf("activate memory template: %w", err)
			}
			// 初始化成功后写标记(模板已灌入); 写失败返回错误, 下次以
			// "memory 非空 + 无标记"路径补标记, 不会重灌模板。
			if err := writeMemoryInitMarker(ws); err != nil {
				return WorkspaceDirs{}, err
			}
		} else if len(entries) > 0 && !markerExists {
			// 老版本已初始化(模板已灌但无标记): 只补标记, 不重灌。
			if err := writeMemoryInitMarker(ws); err != nil {
				return WorkspaceDirs{}, err
			}
		}
		// marker 存在: 已初始化; 用户清空 memory 也不再重灌。
	}
	return dirs, nil
}

// copyTree copies src directory contents into dst (dst must exist).
// 嵌套目录同样 chown 到共享组并保留 setgid, 保证 Runner 新建文件可被 Platform 读取。
func copyTree(src, dst string, uid, gid, shareGID int) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := ensureDirPathNoSymlink(dst, dstPath); err != nil {
				return err
			}
			if err := os.MkdirAll(dstPath, workspaceDirsMode); err != nil {
				return err
			}
			if err := copyTree(srcPath, dstPath, uid, gid, shareGID); err != nil {
				return err
			}
			if os.Geteuid() == 0 {
				if err := os.Chown(dstPath, uid, shareGID); err != nil {
					return err
				}
				if err := os.Chmod(dstPath, workspaceDirsMode); err != nil {
					return err
				}
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
			if err := os.Chown(dstPath, uid, shareGID); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeConfigFiles atomically writes the control-plane files (mTLS material,
// policy manifest) into workspaces/<hash>/config/g<generation>/. File names are
// restricted to plain base names so nothing can escape the config directory.
// 审查 C1/I6: config 按 generation 隔离——旧 generation 容器销毁后的清理
// 只删自己的子目录, 不得影响已创建的新 generation 配置(否则新 Runner
// 丢失 mTLS 材料或挂载已 unlink 目录)。
func writeConfigFiles(root, workspaceHash string, generation uint64, files map[string][]byte, uid, gid, shareGID int) error {
	if len(files) == 0 {
		return nil
	}
	if generation == 0 {
		return fmt.Errorf("config generation must be positive")
	}
	if shareGID <= 0 {
		shareGID = gid
	}
	dir := filepath.Join(root, workspaceHash, "config", fmt.Sprintf("g%d", generation))
	if err := os.MkdirAll(dir, workspaceDirsMode); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	// Runner 以只读挂载 config, 但目录执行/读权限必须允许 Runner 读文件:
	// 属主固定为 Runner uid + 共享组(setgid 继承组)。
	if os.Geteuid() == 0 {
		if err := os.Chown(dir, uid, shareGID); err != nil {
			return fmt.Errorf("chown config dir: %w", err)
		}
		if err := os.Chmod(dir, workspaceDirsMode); err != nil {
			return fmt.Errorf("chmod config dir: %w", err)
		}
	}
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
		if err := os.Chmod(tmpName, workspaceFilesMode); err != nil {
			return fmt.Errorf("chmod %s: %w", name, err)
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(tmpName, uid, shareGID); err != nil {
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

// writeMemoryInitMarker 写入 memory 初始化标记(round11 审查 I7)。
func writeMemoryInitMarker(ws string) error {
	marker := filepath.Join(ws, memoryInitMarkerName)
	if err := os.WriteFile(marker, []byte("initialized\n"), 0o600); err != nil {
		return fmt.Errorf("write memory init marker: %w", err)
	}
	return nil
}
