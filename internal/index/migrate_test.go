package index

import (
	"os"
	"path/filepath"
	"testing"
)

// MigrateIndexFile 是 TODO-21 索引文件物理迁移的核心:改名保数据、幂等可重入、
// 目标已存在时不动旧文件。
func TestMigrateIndexFile(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "index.db")
	newPath := filepath.Join(dir, "index-abcd1234abcd1234.db")

	// 旧文件:建库写一行可辨识数据(folders 根目录恒在,改根名即可)
	db, err := Open(oldPath)
	if err != nil {
		t.Fatalf("Open(legacy): %v", err)
	}
	if _, err := db.Exec(`UPDATE folders SET name = '标记数据' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if err := MigrateIndexFile(oldPath, newPath); err != nil {
		t.Fatalf("MigrateIndexFile: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("旧文件应已改名,stat err = %v", err)
	}
	ndb, err := Open(newPath)
	if err != nil {
		t.Fatalf("Open(new): %v", err)
	}
	var name string
	if err := ndb.QueryRow(`SELECT name FROM folders WHERE id = 1`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	ndb.Close()
	if name != "标记数据" {
		t.Fatalf("数据应随文件迁移,根目录名 = %q", name)
	}

	// 幂等:旧文件已不在,重跑 no-op
	if err := MigrateIndexFile(oldPath, newPath); err != nil {
		t.Fatalf("重跑应 no-op: %v", err)
	}

	// 目标已存在:不动旧文件(残留场景,绝不静默删数据)
	if err := os.WriteFile(oldPath, []byte("残留旧库"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MigrateIndexFile(oldPath, newPath); err != nil {
		t.Fatalf("目标存在时应 no-op: %v", err)
	}
	b, err := os.ReadFile(oldPath)
	if err != nil || string(b) != "残留旧库" {
		t.Fatalf("旧文件必须原样保留,b = %q, err = %v", b, err)
	}
}
