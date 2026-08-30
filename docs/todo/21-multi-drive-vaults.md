# TODO-21 — 多网盘多库(一盘一库,共用 keyfile)

> 状态:进行中(2026-08-30 用户需求驱动)
> 来源:设置页 WebDAV 只能填一套;用户要"每个网盘对应一个库"的分库语义

## 语义定案(与用户逐条确认)

- **一盘一库**:每个网盘档案 = 一个独立库(自己的 blobs + 自己的 index.enc +
  自己的本地索引文件 `index-<盘ID>.db`);**共用同一把本地 keyfile**——
  同一口令、同一把 MK,切盘不重新解锁
- **config schema v2**:`drives[]`(id/name/url/username/password/root_path/
  remember_password)+ `active`(盘 ID);Settings 保持全局并移出
  remember_password(每盘各自决定密码是否落盘)。旧格式首次 Load 自动迁移
  并**立即落盘钉死盘 ID**(ID 随机生成,靠落盘保证稳定;Save 原子写,
  迁移可重入);索引文件随迁:先 Open+Close 做 WAL checkpoint 再改名
  `index.db → index-<ID>.db`,崩溃后下次启动按"新配置 + 旧文件在"补迁
- **切换 = 换库**:要求传输管线空闲(有在途任务直接报错,不静默取消);
  切换前旧盘**尽力而为补一次备份**(revision 落后时);然后关旧库、开新库、
  按新盘重建 store 与传输管线(Manager 的 DB 是直接引用,不做热换)、
  lastBackupRev 按新库 revision 重设;**保持解锁态**(MK 共用);派发
  `drive:switched`,前端回根目录清选择集
- **开新库分支**(CreateAccount/init):本地已有 keyfile 时,建账户 =
  验口令(必须沿用现有口令,共用 keyfile 的前提)→ 推现有 keyfile 到新盘 →
  全新本地索引(旧索引文件非空则归档 backups/);远端已有 keyfile 一律拒绝
  (走解锁/恢复)。本地无 keyfile 才走"生成新密钥"(首次建库)
- **改口令多盘扇出**:Rewrap 一次本地 keyfile,向所有**拿得到密码**的盘
  推送(RememberPassword 且有密码;没记密码的盘跳过并在结果里报告)。
  扇出失败的盘,远端 keyfile 停在旧口令——只影响该盘的新设备恢复
  (需旧口令),本机不受影响(本地 keyfile 已换新,MK 相同)
- **查重与删除**:添加/编辑档案按 URL+用户名+RootPath 查重拒绝(两档案指向
  同一远端库会造出两个索引互踩 LWW);删除档案拒绝活动盘与最后一个盘;
  删除保留本地索引文件(防误删,确认框提示位置)

## 边界(明示不解决)

- **不同 MK 的盘接入无从前置校验**:不拿到口令解不开 keyfile,比不出 MK;
  错配在解密时以 AEAD/AUTH_FAILED 暴露。共用 keyfile 是使用纪律:
  多库都用同一把口令经本机建立/恢复
- RootPath 是盘档案字段 → 同一网盘账号用不同根目录(`/kist-a`、`/kist-b`)
  可开多库,属特性不属 bug
- 切换盘后若该盘远端 index.enc 比本地新,由既有"从远端恢复"(LWW)路径兜底,
  不自动拉取

## 否决备选:单表 + belong 列(评估记录)

直觉上"一张表加归属列"更简单,实际更贵且自毁目标性质:
- 备份/恢复是**文件级**原语(VACUUM INTO 整库快照 → 整文件加密;恢复 =
  ReplaceWith 整文件替换)——单表后 A 盘 index.enc 要么携带全部盘的元数据
  (恢复时抹掉别盘的行),要么新造行级导出/导入层替换这两个实战原语,
  "导出=一个文件"从免费变造轮子
- internal/index 2919 行 75 条 SQL 8 张表全部穿透 drive_id;revision 拆
  每盘计数器写穿全包;漏一条 WHERE 的 gc 孤儿判定会**删掉别盘 blob**——
  分文件布局下此类错误构造上不可能
- 分文件方案 internal/index 零改动;全部状态仍可整目录(KIST_HOME)带走

## 涉及模块

`internal/config`(schema v2 + 迁移 + 索引文件随迁 + 盘 ID/查重)、
`app.go`(+ 生命周期:按活动盘开库/切换/开新库分支/改口令扇出)、
`cmd/kistctl`(drive list/use;config set/migrate --switch 改写活动盘)、
GUI `Settings.vue`(档案列表:增删改/设当前/测试)+ `Lock.vue`(显示当前盘)+
`store.ts` 绑定与 `drive:switched` 处理;`internal/index`、传输管线零改动。

## 验收标准

- 旧 config.json 首次启动自动迁移:drives[0] 承接原配置,盘 ID 落盘稳定,
  index.db 改名为 index-<ID>.db,数据原样可见
- 设置页可添加多个网盘档案(查重生效)、编辑凭据、测试连接、删除(边界拒绝)、
  一键切换;切换要求空闲,切换后文件页回根目录且展示新盘内容
- 同一账号不同 RootPath 开两库互不可见;各自备份各自的 index.enc
- 已有 keyfile 时在第二块盘建账户:不生成新密钥,推现有 keyfile;口令不符报
  AUTH_FAILED
- 改口令后所有记了密码的盘远端 keyfile 同步;未记密码的盘在结果中报告
- CLI:drive list/use;config set 仍可用(编活动盘);migrate --switch 不破
- 全量验收命令全绿;e2e 不改过

## 工作量

中(Go ~400 行 + 前端 ~200 行 + 测试 ~200 行)。
