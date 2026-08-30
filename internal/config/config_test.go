package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// newHome 每个用例独立的 KIST_HOME,杜绝用例间串味。
func newHome(t *testing.T) {
	t.Helper()
	t.Setenv("KIST_HOME", t.TempDir())
}

func TestLoadMissingFile(t *testing.T) {
	newHome(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Drives) != 0 || c.Active != "" {
		t.Fatalf("空目录应得空配置,得 %+v", c)
	}
	if c.Settings.Concurrency != 2 || c.Settings.ChunkMiB != 4 || c.Settings.SizePadding != "on" || c.Settings.OutboxPushFail != "keep" {
		t.Fatalf("默认值不对: %+v", c.Settings)
	}
	if c.Configured() {
		t.Fatal("空配置不应 Configured")
	}
}

// 旧单盘格式(v1)首次 Load:字段承接 + 立即落盘钉死 ID + 幂等(ID 稳定)。
func TestLegacyMigration(t *testing.T) {
	newHome(t)
	legacy := `{
	  "url": "https://dav.example.com/dav",
	  "username": "alice",
	  "password": "pw-secret",
	  "root_path": "/kist",
	  "settings": {"concurrency": 3, "chunk_mib": 8, "remember_password": true,
	               "auto_backup": true, "outbox_push_fail": "keep", "size_padding": "off"}
	}`
	if err := os.WriteFile(ConfigPath(), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("Load(legacy): %v", err)
	}
	if len(c.Drives) != 1 {
		t.Fatalf("应迁移出 1 个盘,得 %d", len(c.Drives))
	}
	d := c.Drives[0]
	if d.URL != "https://dav.example.com/dav" || d.Username != "alice" || d.Password != "pw-secret" ||
		d.RootPath != "/kist" || !d.RememberPassword {
		t.Fatalf("字段承接不完整: %+v", d)
	}
	if d.Name != "dav.example.com" {
		t.Fatalf("展示名应兜底为 URL host,得 %q", d.Name)
	}
	if c.Active != d.ID {
		t.Fatalf("active 应指向迁移盘: %q vs %q", c.Active, d.ID)
	}
	if c.Settings.Concurrency != 3 || c.Settings.ChunkMiB != 8 || c.Settings.SizePadding != "off" || !c.Settings.AutoBackup {
		t.Fatalf("全局 Settings 承接不完整: %+v", c.Settings)
	}
	// 落盘:v1 键不应残留,ID 必须已固定
	b, err := os.ReadFile(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe["drives"]; !ok {
		t.Fatal("迁移后文件应为 v2 形态(有 drives 键)")
	}
	if _, ok := probe["url"]; ok {
		t.Fatal("迁移后不应残留 v1 顶层 url 键")
	}
	pinned := d.ID
	c2, err := Load()
	if err != nil {
		t.Fatalf("二次 Load: %v", err)
	}
	if c2.Drives[0].ID != pinned || c2.Active != pinned {
		t.Fatalf("盘 ID 必须跨 Load 稳定: %s vs %s", c2.Drives[0].ID, pinned)
	}
}

// v2 往返:Save → Load 不丢字段、不重生成 ID、active 保持。
func TestV2RoundTrip(t *testing.T) {
	newHome(t)
	c := &StoredConfig{}
	c.Drives = append(c.Drives,
		Drive{ID: NewDriveID(), Name: "主力", URL: "https://a.example.com/dav", Username: "u1",
			Password: "p1", RootPath: "/kist", RememberPassword: true},
		Drive{ID: NewDriveID(), Name: "备盘", URL: "https://b.example.com/dav", Username: "u2",
			Password: "p2", RootPath: "/kist-b", RememberPassword: false},
	)
	c.Active = c.Drives[1].ID
	c.Settings.AutoBackup = true
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Active != c.Drives[1].ID {
		t.Fatalf("active 应保持: %q", got.Active)
	}
	if len(got.Drives) != 2 {
		t.Fatalf("应有 2 个盘,得 %d", len(got.Drives))
	}
	if got.Drives[0].Password != "p1" {
		t.Fatal("记住密码的盘应保留密码")
	}
	if got.Drives[1].Password != "" {
		t.Fatal("未记住密码的盘密码必须清空不落盘")
	}
	if got.Drives[1].Name != "备盘" {
		t.Fatalf("已命名盘不应被 host 兜底覆盖: %q", got.Drives[1].Name)
	}
}

func TestNormalize(t *testing.T) {
	newHome(t)
	c := &StoredConfig{}
	c.Drives = []Drive{
		{URL: "https://x.example.com/dav", Username: "u"}, // 无 ID 无名无 root
		{ID: "aaaa", URL: "https://y.example.com/dav", Username: "v"},
	}
	c.Active = "不存在" // 悬空 active 应回退第一个
	c.normalize()
	if c.Active != c.Drives[0].ID {
		t.Fatalf("悬空 active 应回退第一个盘: %q", c.Active)
	}
	if c.Drives[0].ID == "" || len(c.Drives[0].ID) != 16 {
		t.Fatalf("缺 ID 应补 16 hex,得 %q", c.Drives[0].ID)
	}
	if c.Drives[0].RootPath != "/kist" {
		t.Fatalf("RootPath 应补默认 /kist,得 %q", c.Drives[0].RootPath)
	}
	if c.Drives[0].Name != "x.example.com" {
		t.Fatalf("名称应兜底为 host,得 %q", c.Drives[0].Name)
	}
	// 无盘:active 清空
	e := &StoredConfig{}
	e.normalize()
	if e.Active != "" {
		t.Fatalf("无盘 active 应为空,得 %q", e.Active)
	}
}

func TestFindDuplicate(t *testing.T) {
	c := &StoredConfig{Drives: []Drive{
		{ID: "a", URL: "https://a.com/dav", Username: "u", RootPath: "/kist"},
		{ID: "b", URL: "https://a.com/dav", Username: "u", RootPath: "/kist-b"}, // 同账号不同根目录:两个库
	}}
	c.Active = "a"
	if d := c.FindDuplicate(&Drive{ID: "new", URL: "https://a.com/dav", Username: "u", RootPath: "/kist"}); d == nil {
		t.Fatal("同 URL+用户名+RootPath 应判重(命中 a)")
	}
	if d := c.FindDuplicate(&Drive{ID: "new", URL: "https://a.com/dav", Username: "u", RootPath: "/kist-b"}); d == nil {
		t.Fatal("与 b 完全相同也应判重(命中 b)")
	}
	if d := c.FindDuplicate(&Drive{ID: "new", URL: "https://a.com/dav", Username: "u", RootPath: "/kist-c"}); d != nil {
		t.Fatalf("同账号不同 RootPath 不应判重,得 %+v", d)
	}
	if d := c.FindDuplicate(&Drive{ID: "a", URL: "https://a.com/dav", Username: "u", RootPath: "/kist"}); d != nil {
		t.Fatal("应排除自身(编辑场景)")
	}
}

func TestDriveIndexPath(t *testing.T) {
	newHome(t)
	if filepath.Base(DriveIndexPath("x")) != "index-x.db" {
		t.Fatalf("文件名形态不对: %s", DriveIndexPath("x"))
	}
	if filepath.Base(LegacyIndexPath()) != "index.db" {
		t.Fatalf("旧路径形态不对: %s", LegacyIndexPath())
	}
}
