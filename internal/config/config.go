package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// HomeDir 返回 kist 数据根目录:KIST_HOME 环境变量优先,
// 否则 os.UserConfigDir()/kist(Linux ~/.config/kist,Windows %AppData%\kist)。
// 目录不存在则创建;测试与多设备模拟靠 KIST_HOME 切换。
func HomeDir() string {
	base := os.Getenv("KIST_HOME")
	if base == "" {
		d, err := os.UserConfigDir()
		if err != nil {
			d = os.TempDir() // 极端环境退化为临时目录,保证可用
		}
		base = filepath.Join(d, "kist")
	}
	_ = os.MkdirAll(base, 0o700)
	return base
}

func ConfigPath() string  { return filepath.Join(HomeDir(), "config.json") }
func KeyFilePath() string { return filepath.Join(HomeDir(), "keyfile") }
func IndexPath() string   { return filepath.Join(HomeDir(), "index.db") }
func BackupDir() string   { return filepath.Join(HomeDir(), "backups") }

// Settings 是行为偏好;改动并发数对已运行的传输在下一个任务生效(调度器每轮重读)。
type Settings struct {
	Concurrency      int  `json:"concurrency"`       // 并发 worker 数,1–4
	ChunkMiB         int  `json:"chunk_mib"`         // 加密分块大小(MiB),0 = 默认 4
	RememberPassword bool `json:"remember_password"` // 显式勾选才把 WebDAV 密码落盘
	AutoBackup       bool `json:"auto_backup"`       // 索引变更后自动云备份(GUI 阶段生效)
}

// StoredConfig 是 config.json 的形态;Password 仅在 RememberPassword 时保留。
type StoredConfig struct {
	URL      string   `json:"url"`
	Username string   `json:"username"`
	Password string   `json:"password,omitempty"`
	RootPath string   `json:"root_path"` // 默认 /kist
	Settings Settings `json:"settings"`
}

func (c *StoredConfig) normalize() {
	if c.RootPath == "" {
		c.RootPath = "/kist"
	}
	if c.Settings.Concurrency < 1 || c.Settings.Concurrency > 4 {
		c.Settings.Concurrency = 2
	}
	if c.Settings.ChunkMiB <= 0 {
		c.Settings.ChunkMiB = 4
	}
}

// Load 读取 config.json;文件不存在时返回带默认值的空配置(不算错误)。
func Load() (*StoredConfig, error) {
	c := &StoredConfig{}
	c.normalize()
	b, err := os.ReadFile(ConfigPath())
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("config: 解析 %s: %w", ConfigPath(), err)
	}
	c.normalize()
	return c, nil
}

// Save 原子写入(tmp+rename);未勾选"记住密码"时密码不落盘。
func Save(c *StoredConfig) error {
	c.normalize()
	if !c.Settings.RememberPassword {
		c.Password = ""
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	p := ConfigPath()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
