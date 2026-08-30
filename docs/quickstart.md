# kist 快速上手(CLI + GUI)

## 这是什么

把本地文件/文件夹加密后上传到任意 WebDAV 网盘,本地维护明文索引便于查找;下载自动解密。所有加密数据共用一个口令派生的主密钥,网盘上看不到文件名、目录结构与内容。

- **本地数据**(Linux `~/.config/kist`,Windows `%AppData%\kist`):
  `config.json`(网盘档案与偏好)、`keyfile`(主密钥包装,所有库共用)、
  `index-<盘ID>.db`(明文索引,每个网盘一个)、
  `outbox/`(出站箱:待上传的加密产物)、`covers/`(封面缓存,GUI 自动管理)、
  `backups/`(恢复归档)、`kist.log`(运行日志)
- **网盘上的形态**:根目录 `/kist/` 下只有随机名加密 blob、`keyfile`、`index.enc`,
  另有 `/kist/covers/` 封面子目录(同样是随机名加密对象,TODO-10),无任何明文信息
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

## 多网盘:一盘一库

可以保存多个网盘档案,**每个网盘是一个独立的库**(各自的文件列表与各自的
远端备份),共用同一把 keyfile——所有库是同一个加密口令,切换网盘不用重新解锁:

```bash
kistctl drive list                # 列出档案(* = 当前;名称/ID/序号均可识别)
kistctl drive use 2               # 切换当前库所在盘(下次命令起生效)
echo "网盘密码" | kistctl config set --url <URL> --user 用户名 --name 名字 --pass-stdin
                                  # 配置当前盘;无档案时创建第一个
```

GUI 设置页提供同样的管理:添加/编辑/删除档案、测试连接、一键切换。

- **同一账号开两个库**:用不同「远端根目录」(如 `/kist-a` 与 `/kist-b`)即可
- **切换要求空闲**:有在途传输会被拒绝;切换前当前盘落后的索引会先补一次备份
- **第二块盘"建账户"** = 推入现有 keyfile(口令必须一致),不会生成新密钥;
  本地残留的旧索引会先归档到 `backups/`
- **改口令**向所有记了密码的盘同步 keyfile;没记密码的盘保持旧口令
  (新设备恢复它们时需旧口令,报告里会提示)
- 多库之间数据互不可见;搬家用 `migrate`,切换不搬数据

## 日常使用

```bash
export KIST_PASS='我的加密口令'   # 设了它就不用每次 --pass-stdin

kistctl put ~/照片 --dest /2026          # 加密上传文件夹(按叶子目录打包,见下节)
kistctl put 报告.pdf --dest /工作
kistctl put 大视频.mp4 --dest /视频 --defer   # 弱网大文件:只加密+记账不立即上传(见下节)
kistctl ls /2026                          # 列目录(目录行附子树摘要,见下下节)
kistctl search 照片                        # 搜文件名、备注、目录名与目录 tag
kistctl info 5                            # 查明细:加密/上传时间、SHA、备注、封面
kistctl note 5 --set "海边旅行"            # 写备注(可被搜索)
kistctl get 5 --to ~/Downloads            # 下载自动解密(校验不过不会落盘)
kistctl rm 5 && kistctl gc                # 删除 → 清理远端 blob
kistctl backup                            # 把索引加密备份到网盘(重要!定期执行)
```

## 文件夹打包:一话一对象

`put` 文件夹默认**按叶子目录打包**:不含子目录的文件夹整体压成一个 zip
加密对象(Store 方式不再压缩——图片本就压过),下载时自动解压还原成
文件夹。粒度规则只看「有无子目录」,不看内容不看名字:

```
作品A/第01话/(散图)   → 一个加密对象   ← 一话一对象,远端只见随机名 blob
作品A/第02话/(散图)   → 一个加密对象
作品A/说明.txt        → 独立小文件入库
```

- 多话作品:`put 作品A --dest /漫画` → `/漫画/作品A/` 下每话一条记录,
  `ls` 以 `P 名称/` 显示;补一话就再 put 一次,已有各话不动
- 单话作品:`put 作品B --dest /漫画/作品B` → 目录下一个打包条目
- `--expand` 恢复逐文件旧行为(每文件一个对象,文件夹镜像为虚拟目录)
- `get <id>` 默认还原成文件夹;`--keep-zip` 直接落 `<名>.zip`,
  想喂漫画阅读器自己改名 `.cbz` 即可
- 打包时顺带抽词法序第一张图当封面缩略图(`info` 可见)

边界:文件名必须合法 UTF-8;symlink/FIFO 等特殊文件不支持——遇到会
**整次 put 拒绝**并列出全部问题路径(先用 `convmv -f <编码> -t utf8 -r
--notest` 转码、清理特殊文件后再传),不会静默跳过丢数据。

## 作品级元数据与浏览(meta / mv)

浏览的决策单元是**作品**,不是话:`ls` 的目录行自带子树摘要,
作者/tag/封面挂在目录**或文件**上(epub 等单文件作品同样可挂),
归属传错了有便宜的纠错通道。

```bash
kistctl ls /漫画                          # 目录行附子树摘要:
                                          #   D 作品A/  12 话 · 8.2GB · ← 08-01,待传 2
kistctl meta set /漫画/作品A --tag "科幻,已完结" --note "作者:某人"
kistctl meta set /漫画/作品A --cover 7    # 封面指向一个已上传的文件 id
                                          # (惯例:put cover.jpg 后引用之;传 0 清除)
kistctl meta set /漫画/作品A              # 不带 flag = 查看当前元数据
kistctl meta set 3f9c --tag "作者:某人"    # 文件形态的作品:目标用 uuid|id,
                                          # --note/--tag 语义同目录(--cover 仅目录)
kistctl meta list                         # 列出全部带元数据的目录
kistctl search 科幻                        # 目录与文件的 名/tag/备注 都搜得到

kistctl mv 5 /漫画/作品B                  # 纯索引移动:改挂点不改远端,
                                          # 零流量零重传(重名自动 " (1)" 消解)

kistctl mkdir /书/小说X                  # 先建目录再往里传(多级、幂等,零流量)
kistctl put *.epub --dest /书/小说X      # 批量上传文件:每个文件独立成条——
                                          # 打包只作用于"上传文件夹",传文件从不打包
kistctl rename /书/小说X 小说X-已完结    # 目录重命名(同名幂等;撞名报错不自动消解)
```

- 摘要是纯查询,任何增删之后即时反映;`PackCount>0` 显示"N 话",
  否则"N 个文件"——相册/专辑同一措辞逻辑
- 封面三级回退:自定义封面 → 子条目名称序前四个拼 2×2 宫格(位置即信息,
  空位留白)→ 空作品由 GUI 渲染端按目录名稳定挑占位图;引用悬空自动回退
- 元数据随 `backup`/`pull` 走索引云备份,多设备一致
- 虚拟目录是纯索引概念:mkdir 多级幂等、零远端流量,空目录随备份同步;
  rename 同款零流量(文件**重**命名暂无入口,目录移动亦无——按需再立项);
  GUI 工具栏「新建文件夹」「重命名」同款(当前目录下)
- 文件封面:epub/视频等无自动缩略图的内容,在 GUI 元数据面板「导入封面」
  选本地图片(与上传缩略图同规格);TODO-10 起封面字节存独立加密对象
  (远端 /kist/covers/,本地 GUI 有磁盘缓存),索引只存轻引用,随索引备份同步;
  断网导入自动入出站箱,push 后对其他设备可见;pack 的自动首页封面
  被覆盖后清除不恢复(确认框会提示)

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

## GUI(kist,Phase 7)

CLI 之外的同核心桌面壳:Wails 窗口,四页面(文件/传输/设置)+ 解锁页。
仓库内构建运行:

```bash
make dev      # 开发模式(热重载;= wails dev -tags webkit2_41)
make build    # 发布构建 → build/bin/kist
KIST_HOME="$PWD/.sandbox" ./build/bin/kist   # 沙盒试用 GUI
```

- **首次向导**:填 WebDAV 配置 → 测试连接 → 设加密口令 → 建账户(与 CLI 的
  `config set` + `init` 等价)
- **新设备恢复**:复制 config.json 到新机器后启动,解锁页自动出现
  "从远端恢复"——输口令即拉回 keyfile 与索引(含缩略图/备注),等价 `pull`
- **文件页**:面包屑导航 + 列表/网格双视图(网格目录卡 = 封面宫格)、搜索
  (文件名/备注/目录名/tag,300ms 防抖);工具栏放上传文件/文件夹(默认
  一话一对象)、新建文件夹、下载与视图切换;**右键条目呼出操作菜单**——
  重命名(单个目录)/移动/删除(支持先勾选多选再右键)/元数据,右键目标
  未选中时会先选中它;右键空白处无菜单(webkit 原生菜单已全局抑制);
  文件详情含缩略图、加密/上传时间、SHA、备注与 tag;
  目录/文件的"元数据"可编辑 note/tag(目录封面为文件 ID 引用;
  文件封面可导入本地图片)
- **传输页**:进度条/阶段/取消;在途传输关窗会先询问
- **设置页**:网盘档案列表(增/删/改/一键切换/测试连接)、并发(1–4)/块大小、
  自动备份开关(索引变更后静默 30s 自动备份,退出前有未备份变更也会补一次)、
  立即备份、孤儿清理(预览→确认两步)、改口令(多盘同步 keyfile)、锁定
- **GUI 不做**的:出站箱(`--defer`/push/verify)与网盘间迁移(migrate)仍走 CLI
- ⚠️ **GUI 开着时别同时跑 CLI 传输**:两者共用 `/tmp/kist` 临时目录,
  后启动的一方会清掉对方的临时产物(任一方单独使用没问题)

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
- `kistctl gc --dry-run`:查看 trash 与孤儿 blob(文件孤儿与封面孤儿分账),确认后再实删
- 被替换/覆盖的旧索引都在本地 `backups/` 目录,可手动恢复
- 口令错了会明确报"口令错误",不会误伤数据

## 封面出库迁移(TODO-10)

早期版本把封面字节存在索引库里(库体积随图片数膨胀、每次备份都背全部封面)。
当前版本已改为「一封面一 blob」:封面存远端 `/kist/covers/`,索引只存轻引用。
**旧库升级后跑一次迁移**即可把存量缩略图出库并回收库空间:

```bash
kistctl covers migrate --dry-run             # 先看规模:多少行、多大体积、预计时长
kistctl covers migrate --pass-stdin          # 实跑:逐行加密上传,断点续跑
kistctl covers migrate --max 500 --pass-stdin # 受网盘限速时分批跑(~100 请求/分钟)
kistctl backup --pass-stdin                  # 迁移后备份一次,把封面引用同步到远端
```

- 迁移行按用户内容保守保护(记为 custom):gc 永不自动删除孤儿封面
- 结束会 VACUUM 回收库空间;GUI 若同开一库请先退出再跑
- 新上传的图片自动走新管线,无需任何操作

## 当前边界

- GUI 已覆盖日常使用(浏览/搜索/上传/下载/元数据/移动/备份/恢复);出站箱与迁移仍是 CLI 专属
- GUI 的自动备份仅 GUI 会话内生效;纯 CLI 使用仍需手动 `backup`
- 视频/epub 无自动缩略图(GUI 元数据面板可手动导入封面);封面已出库为独立对象:
  每张图片上传的请求数 +1(受网盘限速影响,批量导入耗时近似翻倍),
  GUI 取未缓存封面需联网(已缓存或 legacy 行零网络);密文大小已量化到档位(默认开):≤1MiB 文件 4KiB 粒度、大文件 10% 阶梯,
  网盘只能推断大致量级;流量敏感可在 config.json 设 `"size_padding": "off"`(新上传退回精确大小)
- 无断点续传(当前网盘 WebDAV 不支持 Range):大文件建议 `put --defer` 走出站箱;
  直接 put 失败会整体重传,且重试不免重新加密
- 文件夹打包的往返是「结构级」而非字节级镜像:文件字节与文件级 mtime 一致,
  目录自身 mtime 与权限位不保证;混杂层的散文件独立入库(不进包)
- 多网盘(一盘一库)共用 keyfile 是使用纪律:接入远端 keyfile 与本地不同源
  的盘没有前置校验,错配在解密时报"密钥不符";切换盘后若该盘远端备份比本地新,
  需手动"从远端恢复"(LWW),不自动拉取
