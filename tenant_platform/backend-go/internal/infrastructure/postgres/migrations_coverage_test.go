package postgres

import (
	"os"
	"testing"
)

// TestMigrationFilesCoverPendingMigrations: migrationFiles()（目录真值）与
// pendingMigrations（已有 schema 的补跑清单）必须互相覆盖。
//
// 背景（2026-09-14 实际踩到）: 新增迁移只放进目录、忘了加进 pendingMigrations 时，
// **已有 schema 的部署永远拿不到该迁移**，而全新库/fresh EnsureSchema 又是正常的
// ——这种"只有存量环境坏"的静默故障最难发现（当时表现为 CI 里 0059 的旧 CHECK
// 约束还在，新能力值写入直接 23514，而且是先推送再被 CI 抓到）。本测试把它变成硬失败。
func TestMigrationFilesCoverPendingMigrations(t *testing.T) {
	files := migrationFiles()
	if len(files) == 0 {
		t.Fatalf("migrationFiles() is empty (migrations dir: %s)", migrationsDir())
	}
	pending := make(map[string]struct{}, len(pendingMigrations))
	for _, migration := range pendingMigrations {
		pending[migration.file] = struct{}{}
	}
	for _, name := range files {
		if name == "0001_foundation.sql" {
			continue // 基础迁移由 EnsureSchema 的 fresh / drop 分支处理
		}
		if _, ok := pending[name]; !ok {
			t.Errorf("迁移 %s 在目录里但不在 pendingMigrations 中: "+
				"已有 schema 的部署不会补跑它（必须加进 pendingMigrations）", name)
		}
	}
	onDisk := make(map[string]struct{}, len(files))
	for _, name := range files {
		onDisk[name] = struct{}{}
	}
	for _, migration := range pendingMigrations {
		if _, ok := onDisk[migration.file]; !ok {
			t.Errorf("pendingMigrations 列了 %s 但磁盘上没有该文件", migration.file)
		}
	}
}

// TestMigrationFilesOnDiskSortedAndConventional: 目录是单一真值 ⇒ 目录里只应有合规
// 命名的迁移文件，且应用顺序 = 文件名字典序（四位数序号保证）。
func TestMigrationFilesOnDiskSortedAndConventional(t *testing.T) {
	entries, err := os.ReadDir(migrationsDir())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !migrationFilePattern.MatchString(entry.Name()) {
			t.Errorf("迁移目录出现不合规命名的文件(约定 %s): %s", migrationFilePattern, entry.Name())
			continue
		}
		count++
	}
	files := migrationFiles()
	if count != len(files) {
		t.Fatalf("migrationFiles() = %d 条, 目录有 %d 个 .sql", len(files), count)
	}
	for i := 1; i < len(files); i++ {
		if files[i-1] >= files[i] {
			t.Fatalf("迁移未按字典序排列: %s >= %s", files[i-1], files[i])
		}
	}
}
