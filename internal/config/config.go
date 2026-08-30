package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
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
func BackupDir() string   { return filepath.Join(HomeDir(), "backups") }

// OutboxDir 出站箱(TODO-13):put --defer 留下的加密产物,待 push/手工搬运。
func OutboxDir() string { return filepath.Join(HomeDir(), "outbox") }

// LegacyIndexPath 旧单库索引文件(schema v1 的 index.db)。
// 仅迁移与残留判断用;正常布局下活动库在 DriveIndexPath。
func LegacyIndexPath() string { return filepath.Join(HomeDir(), "index.db") }

// DriveIndexPath 指定盘的库索引文件:一盘一库(TODO-21),盘 ID 钉在文件名上。
// ID 由 config 落盘钉死,文件名因此稳定——这是"索引随盘切换"的物理基础。
func DriveIndexPath(driveID string) string {
	return filepath.Join(HomeDir(), "index-"+driveID+".db")
}

// Settings 是全局行为偏好;改动并发数对已运行的传输在下一个任务生效
// (调度器每轮重读)。注意:WebDAV 密码是否落盘(RememberPassword)是
// 每盘自己的属性(Drive 字段),不在这里。
type Settings struct {
	Concurrency    int    `json:"concurrency"`      // 并发 worker 数,1–4
	ChunkMiB       int    `json:"chunk_mib"`        // 加密分块大小(MiB),0 = 默认 4
	AutoBackup     bool   `json:"auto_backup"`      // 索引变更后自动云备份(GUI 阶段生效)
	OutboxPushFail string `json:"outbox_push_fail"` // push 失败政策:keep(默认,挂账)|discard(删行+产物)
	SizePadding    string `json:"size_padding"`     // 大小量化填充:on(默认,v2 档位)|off(v1,流量敏感网盘可选)
}

// Drive 是单个网盘档案 = 一个独立库(自己的 blobs + 自己的 index.enc +
// 自己的本地索引文件),共用同一把本地 keyfile(TODO-21 语义定案)。
type Drive struct {
	ID               string `json:"id"`   // 随机 16 hex,生成后不变(索引文件名锚点)
	Name             string `json:"name"` // 展示名;空则 normalize 用 URL host 补
	URL              string `json:"url"`
	Username         string `json:"username"`
	Password         string `json:"password,omitempty"` // 仅 RememberPassword 时保留
	RootPath         string `json:"root_path"`          // 默认 /kist;同账号不同根目录可开多库
	RememberPassword bool   `json:"remember_password"`
}

// StoredConfig 是 config.json 的形态(schema v2)。旧单盘格式(顶层
// url/username/... 字段)由 Load 探测并自动迁移落盘,不留双形态兼容面。
type StoredConfig struct {
	Drives   []Drive  `json:"drives"`
	Active   string   `json:"active"` // 当前盘 ID;normalize 保证指向存在的盘
	Settings Settings `json:"settings"`
}

// ActiveDrive 返回当前盘档案;无盘或 ID 悬空(normalize 前的脏数据)返回 nil。
func (c *StoredConfig) ActiveDrive() *Drive {
	return c.DriveByID(c.Active)
}

func (c *StoredConfig) DriveByID(id string) *Drive {
	for i := range c.Drives {
		if c.Drives[i].ID == id {
			return &c.Drives[i]
		}
	}
	return nil
}

// Configured 当前盘是否可用于装配远端客户端(URL+用户名齐全)。
func (c *StoredConfig) Configured() bool {
	d := c.ActiveDrive()
	return d != nil && d.URL != "" && d.Username != ""
}

// FindDuplicate 返回与 d 同 URL+用户名+RootPath 的既有盘(排除 d 自身)。
// 两档案指向同一远端库会造出两个索引互踩 LWW,添加/编辑时必须拒绝。
func (c *StoredConfig) FindDuplicate(d *Drive) *Drive {
	for i := range c.Drives {
		o := &c.Drives[i]
		if o.ID == d.ID {
			continue
		}
		if o.URL == d.URL && o.Username == d.Username && o.RootPath == d.RootPath {
			return o
		}
	}
	return nil
}

// NewDriveID 生成盘 ID(8 字节随机数 hex)。crypto/rand 失败时退化为
// 时间戳(仅失去随机性,不失去唯一性;正常环境不会走到)。
func NewDriveID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return fmt.Sprintf("%x", b)
}

func (c *StoredConfig) normalize() {
	if c.Settings.Concurrency < 1 || c.Settings.Concurrency > 4 {
		c.Settings.Concurrency = 2
	}
	if c.Settings.ChunkMiB <= 0 {
		c.Settings.ChunkMiB = 4
	}
	if c.Settings.OutboxPushFail != "discard" {
		c.Settings.OutboxPushFail = "keep" // 两档之外的值一律回退 keep(TODO-13)
	}
	if c.Settings.SizePadding != "off" {
		c.Settings.SizePadding = "on" // 默认量化(TODO-08),仅显式 off 才关闭
	}
	// 盘档案:ID 兜底(手工编辑丢 ID 的容错)、RootPath 默认、名称兜底。
	// 注意密码不在这里清(只读路径不毁数据),落盘清理由 Save 负责。
	for i := range c.Drives {
		d := &c.Drives[i]
		if d.ID == "" {
			d.ID = NewDriveID()
		}
		if d.RootPath == "" {
			d.RootPath = "/kist"
		}
		if d.Name == "" {
			d.Name = hostOf(d.URL)
		}
	}
	// active 必须指向存在的盘:悬空回退第一个;无盘清空。
	if c.ActiveDrive() == nil {
		if len(c.Drives) > 0 {
			c.Active = c.Drives[0].ID
		} else {
			c.Active = ""
		}
	}
}

// hostOf 从 URL 提取主机名作展示名兜底(dav.example.com → "dav.example.com")。
func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "未命名网盘"
}

// Load 读取 config.json;文件不存在时返回带默认值的空配置(不算错误)。
// 旧单盘格式(schema v1)在此自动迁移:识别、转换、立即落盘——盘 ID 必须落盘
// 钉死,否则每次 Load 重新生成 ID,索引文件名就漂移了。落盘失败直接报错,
// 不带病运行(调用方都能处理配置错误)。
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
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, fmt.Errorf("config: 解析 %s: %w", ConfigPath(), err)
	}
	if _, ok := probe["drives"]; ok { // schema v2
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("config: 解析 %s: %w", ConfigPath(), err)
		}
		c.normalize()
		return c, nil
	}
	m, err := migrateLegacy(b)
	if err != nil {
		return nil, err
	}
	if err := Save(m); err != nil {
		return nil, fmt.Errorf("config: 旧格式迁移落盘失败: %w", err)
	}
	return m, nil
}

// migrateLegacy 把 v1 单盘字段转换为 v2 的 drives[0]。索引文件的物理迁移
// 不在这里(config 不碰索引内容),由调用方经 index.MigrateIndexFile 完成。
func migrateLegacy(b []byte) (*StoredConfig, error) {
	var legacy struct {
		URL      string `json:"url"`
		Username string `json:"username"`
		Password string `json:"password"`
		RootPath string `json:"root_path"`
		Settings struct {
			Concurrency      int    `json:"concurrency"`
			ChunkMiB         int    `json:"chunk_mib"`
			RememberPassword bool   `json:"remember_password"`
			AutoBackup       bool   `json:"auto_backup"`
			OutboxPushFail   string `json:"outbox_push_fail"`
			SizePadding      string `json:"size_padding"`
		}
	}
	if err := json.Unmarshal(b, &legacy); err != nil {
		return nil, fmt.Errorf("config: 解析旧格式 %s: %w", ConfigPath(), err)
	}
	c := &StoredConfig{
		Drives: []Drive{{
			ID:               NewDriveID(),
			Name:             hostOf(legacy.URL),
			URL:              legacy.URL,
			Username:         legacy.Username,
			Password:         legacy.Password,
			RootPath:         legacy.RootPath,
			RememberPassword: legacy.Settings.RememberPassword,
		}},
		Settings: Settings{
			Concurrency:    legacy.Settings.Concurrency,
			ChunkMiB:       legacy.Settings.ChunkMiB,
			AutoBackup:     legacy.Settings.AutoBackup,
			OutboxPushFail: legacy.Settings.OutboxPushFail,
			SizePadding:    legacy.Settings.SizePadding,
		},
	}
	c.Active = c.Drives[0].ID
	c.normalize()
	return c, nil
}

// Save 原子写入(tmp+rename);未勾选"记住密码"的盘密码不落盘(逐盘判断)。
func Save(c *StoredConfig) error {
	c.normalize()
	for i := range c.Drives {
		if !c.Drives[i].RememberPassword {
			c.Drives[i].Password = ""
		}
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
