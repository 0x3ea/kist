# kistctl 快速上手(v0.1 CLI)

## 这是什么

把本地文件/文件夹加密后上传到任意 WebDAV 网盘,本地维护明文索引便于查找;下载自动解密。所有加密数据共用一个口令派生的主密钥,网盘上看不到文件名、目录结构与内容。

- **本地数据**(Linux `~/.config/kist`,Windows `%AppData%\kist`):
  `config.json`(网盘配置)、`keyfile`(主密钥包装)、`index.db`(明文索引)、
  `outbox/`(出站箱:待上传的加密产物)、`backups/`(恢复归档)、`kist.log`(运行日志)
- **网盘上的形态**:根目录 `/kist/` 下只有随机名加密 blob、`keyfile`、`index.enc`,无任何明文信息
- **⚠️ 口令是唯一凭证**:忘记口令 = 数据不可恢复,没有任何后门。请牢记并保密

## 安装

```bash
# Linux:dist/ 下的二进制赋予执行权限,建议链接到 PATH
chmod +x dist/kistctl-v0.1.0-linux-amd64
mkdir -p ~/bin && ln -s "$PWD/dist/kistctl-v0.1.0-linux-amd64" ~/bin/kistctl

# Windows:dist/kistctl-v0.1.0-windows-amd64.exe 直接运行
```

## 沙盒试用(推荐先用这个)

不想动 `~/bin` 和 `~/.config/kist`?仓库根提供了包装脚本 `kistctl-sandbox`:
数据全部落在项目内 `.sandbox/`(已 gitignore),命令用法与 `kistctl` 完全相同。
清空重来只需 `rm -rf .sandbox`。

```bash
ln -sf dist/kistctl-v0.1.0-linux-amd64 kistctl   # 仓库根链接(一次性,已 gitignore)
echo "网盘密码" | ./kistctl-sandbox config set --url https://dav.example.com/dav --user 用户名 --pass-stdin
KIST_PASS='我的加密口令' ./kistctl-sandbox init --pass-stdin
```

等价的手写方式是给每条命令加前缀(命令级环境变量,不污染会话):

```bash
KIST_HOME="$PWD/.sandbox" ./kistctl put ~/文件 --dest /测试
```

注意:沙盒与正式模式若连同一个网盘,远端只有一份 keyfile——一边 init 过,另一边直接 pull,不要再 init。

## 首次配置(两个密码,别混淆)

```bash
# ① 网盘密码(WebDAV 账户密码)
echo "网盘密码" | kistctl config set --url https://dav.example.com/dav --user 用户名 --pass-stdin

# ② kist 加密口令(自己起一个,建议 12 字符以上,这是数据的命)
KIST_PASS='我的加密口令' kistctl init --pass-stdin
```

常见网盘提示:
- **坚果云**:网页端「账户信息 → 安全选项 → 添加应用密码」生成专用密码;URL 为 `https://dav.jianguoyun.com/dav/`
- **InfiniCloud**:在应用页面开通 WebDAV,用生成的专用密码
- **自建 Nextcloud**:URL 形如 `https://你的域名/remote.php/dav/files/用户名/`

## 日常使用

```bash
export KIST_PASS='我的加密口令'   # 设了它就不用每次 --pass-stdin

kistctl put ~/照片 --dest /2026          # 加密上传文件夹(递归,图片自动生成缩略图)
kistctl put 报告.pdf --dest /工作
kistctl put 大视频.mp4 --dest /视频 --defer   # 弱网大文件:只加密+记账不立即上传(见下节)
kistctl ls /2026                          # 列目录(待上传文件带「待上传」标记)
kistctl search 照片                        # 搜文件名与备注
kistctl info 5                            # 查明细:加密/上传时间、SHA、备注、缩略图
kistctl note 5 --set "海边旅行"            # 写备注(可被搜索)
kistctl get 5 --to ~/Downloads            # 下载自动解密(校验不过不会落盘)
kistctl rm 5 && kistctl gc                # 删除 → 清理远端 blob
kistctl backup                            # 把索引加密备份到网盘(重要!定期执行)
```

## 弱网 / 大文件:出站箱(put --defer)

很多网盘的 WebDAV 不支持断点续传,大文件上传一旦中断就要整体重传。
出站箱把「加密+记账」与「上传」拆开——加密产物先落本地,运输择机进行:

```bash
KIST_PASS='我的加密口令' kistctl put 大视频.mp4 --dest /视频 --defer
# 产物落在本地 outbox/,索引立即记为「待上传」;此时 get 会提示先完成上传

kistctl outbox list                # 查看待上传对象、对应文件与产物占用
kistctl outbox push --all          # 方式一:网络好时让 kist 自己重传(免重新加密)
kistctl outbox verify              # 方式二收账:用网盘官方客户端把 outbox/ 里的
                                   # 密文文件(保持原文件名)上传到远端 /kist/,
                                   # 再执行本命令核对大小并标记完成
kistctl outbox discard <blob名>    # 放弃一笔:删索引记录与本地产物
```

push 失败默认保留挂账(可重试/手工搬运);想失败即整笔放弃,在
`config.json` 的 settings 里设 `"outbox_push_fail": "discard"`。

## 换网盘 / 网盘到期(migrate)

迁移是纯密文字节搬运:不解锁、不触碰明文,断点可续(中断重跑只补缺失
对象),限速网盘下可跨天慢慢搬,也可以放到 VPS 上跑:

```bash
echo "新网盘密码" | kistctl migrate --url https://新网盘/dav --user 用户名 --pass-stdin
# 结束时自动校验 keyfile/index.enc 双端一致;确认无误后加 --switch 重跑,
# 已完成对象自动跳过(秒级收尾),并把 config 切换到新网盘:
echo "新网盘密码" | kistctl migrate --url https://新网盘/dav --user 用户名 --pass-stdin --switch
```

本地 `keyfile` 与 `index.db` 无需变动;切换后建议尽快在新端 `backup` 一次。

## 多设备 / 换电脑

新设备只需:

```bash
echo "网盘密码" | kistctl config set --url ... --user ... --pass-stdin
KIST_PASS='我的加密口令' kistctl pull --pass-stdin
```

pull 会自动拉取 keyfile 并恢复整个索引(含备注与缩略图)。
**注意 LWW 语义**:同时只在一台设备上写;换设备前先在旧设备 `backup`,新设备先 `pull` 再用。落后一方的本地改动会被归档到 `backups/` 而不是合并。
其他设备同步到的「待上传」文件不可下载,需原设备完成 push/verify。

## 出问题时

- 运行日志在 `KIST_HOME/kist.log`:传输起止、WebDAV 重试、孤儿产生、备份与 pull 决策都有记录
- `kistctl gc --dry-run`:查看 trash 与孤儿 blob,确认后再实删
- 被替换/覆盖的旧索引都在本地 `backups/` 目录,可手动恢复
- 口令错了会明确报"口令错误",不会误伤数据

## 当前边界(v0.1)

- 无 GUI(Phase 7 开发中);无自动备份(记得手动 `backup`)
- 视频无缩略图;密文大小会泄露大致明文大小(所有非填充加密方案的共同边界)
- 无断点续传(当前网盘 WebDAV 不支持 Range):大文件建议 `put --defer` 走出站箱;
  直接 put 失败会整体重传,且重试不免重新加密
