package e2e

// TODO-10 封面出库的行为测试:直传封面落 covers 命名空间且不额外计
// revision;defer/push/verify 全程封面与文件同进退(含 verify 无主清理
// 不误删封面产物);gc 两命名空间分账。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"kist/internal/index"
	"kist/internal/transfer"
)

// TestE2EUploadProducesCoverBlob 直传图片:封面引用 derived/ready、字节在
// covers 命名空间且可解回合法 JPEG;revision 只因上传本身 +1(拆放大器)。
func TestE2EUploadProducesCoverBlob(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	jpg := makeJPEG(t, t.TempDir(), "照片.jpg", 700, 500)

	rev0, _ := e.db.Revision()
	n, err := e.m.UploadPaths(ctx, []string{jpg}, 1)
	if err != nil || n != 1 {
		t.Fatalf("入队 %d: %v", n, err)
	}
	allDone(t, waitIdle(t, e.m))
	rev1, _ := e.db.Revision()
	if rev1 != rev0+1 {
		t.Fatalf("revision 应只 +1(封面引用随上传事务): %d → %d", rev0, rev1)
	}

	hits, _ := e.db.Search("照片.jpg", 5)
	if len(hits) != 1 {
		t.Fatalf("搜索: %+v", hits)
	}
	cov, err := e.db.GetReadyCover(hits[0].ID)
	if err != nil {
		t.Fatalf("封面引用缺失: %v", err)
	}
	if cov.Source != index.CoverDerived || cov.State != index.CoverReady {
		t.Fatalf("应为 derived/ready: %+v", cov)
	}

	// 主命名空间不含封面,covers 命名空间恰一个
	main, _ := e.store.ListBlobs(ctx)
	for _, name := range main {
		if name == cov.BlobName {
			t.Fatal("封面 blob 不应出现在主命名空间")
		}
	}
	covers, err := e.store.ListCoverBlobs(ctx)
	if err != nil || len(covers) != 1 || covers[0].Name != cov.BlobName {
		t.Fatalf("covers 命名空间应恰有封面 blob: %+v %v", covers, err)
	}

	td, err := e.fetchAndDecryptCover(t, cov)
	if err != nil {
		t.Fatalf("封面解密失败: %v", err)
	}
	if len(td) == 0 {
		t.Fatal("封面解密结果为空")
	}
}

// TestE2EDeferCoverThenPush defer 上传图片:封面引用 uploading、封面产物
// 入箱;push 后文件与封面一并收账。
func TestE2EDeferCoverThenPush(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	jpg := makeJPEG(t, t.TempDir(), "挂账.jpg", 400, 300)

	n, err := e.m.DeferPaths(ctx, []string{jpg}, 1)
	if err != nil || n != 1 {
		t.Fatalf("DeferPaths: n=%d err=%v", n, err)
	}
	// defer 的终态是 deferred(记账+入箱),不是 done
	if trs := waitIdle(t, e.m); trs[0].Phase != transfer.PhaseDeferred {
		t.Fatalf("defer 终态 %s(%s)", trs[0].Phase, trs[0].Err)
	}

	hits, _ := e.db.Search("挂账.jpg", 5)
	f, err := e.db.GetFile(hits[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.GetReadyCover(f.ID); err == nil {
		t.Fatal("uploading 封面引用应不可见")
	}
	covers, err := e.db.ListUploadingCovers()
	if err != nil || len(covers) != 1 {
		t.Fatalf("应有一笔封面挂账: %+v %v", covers, err)
	}
	coverRow := covers[0]
	if _, err := os.Stat(transfer.OutboxArtifactPath(coverRow.BlobName)); err != nil {
		t.Fatal("封面产物应在出站箱")
	}

	files, _ := e.db.ListUploading()
	if n := e.m.PushPending(ctx, files, covers); n != 2 {
		t.Fatalf("push 应入队 2 笔(文件+封面): %d", n)
	}
	// 快照里还有先前的 deferred 记录,不能 allDone,等空闲后直接看账
	waitIdle(t, e.m)

	got, _ := e.db.GetFile(f.ID)
	if got.State != "ready" {
		t.Fatalf("push 后文件 state=%s", got.State)
	}
	ready, err := e.db.GetReadyCover(f.ID)
	if err != nil || ready.BlobName != coverRow.BlobName {
		t.Fatalf("push 后封面应 ready: %+v %v", ready, err)
	}
	if ents, _ := os.ReadDir(transfer.OutboxArtifactPath("")); len(ents) != 0 {
		t.Fatalf("出站箱应清空: %v", ents)
	}
}

// TestE2EVerifyKeepsCoverArtifact verify 不搬运也不误删:封面产物必须在
// 双挂账集里(修复无主清理误删);discard 文件账时封面账与产物一并回滚。
func TestE2EVerifyKeepsCoverArtifact(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	jpg := makeJPEG(t, t.TempDir(), "核验.jpg", 300, 300)

	n, _ := e.m.DeferPaths(ctx, []string{jpg}, 1)
	if n != 1 {
		t.Fatalf("DeferPaths: n=%d", n)
	}
	if trs := waitIdle(t, e.m); trs[0].Phase != transfer.PhaseDeferred {
		t.Fatalf("defer 终态 %s(%s)", trs[0].Phase, trs[0].Err)
	}
	hits, _ := e.db.Search("核验.jpg", 5)
	if len(hits) != 1 {
		t.Fatalf("搜索: %+v", hits)
	}
	f, _ := e.db.GetFile(hits[0].ID)
	covers, _ := e.db.ListUploadingCovers()
	if len(covers) != 1 {
		t.Fatalf("封面挂账缺失: %+v", covers)
	}
	coverBlob := covers[0].BlobName

	// 未搬运就 verify:文件与封面都报 missing,但封面产物不得被无主清理删掉
	if _, err := transfer.RunOutboxVerify(ctx, e.store, e.db); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(transfer.OutboxArtifactPath(coverBlob)); err != nil {
		t.Fatalf("封面产物被误删(verify 挂账集未含封面): %v", err)
	}

	// discard 文件账:封面账级联放弃、两个产物一起清
	if err := transfer.OutboxDiscard(e.db, f.BlobName); err != nil {
		t.Fatal(err)
	}
	if has, _ := e.db.HasCover(f.ID); has {
		t.Fatal("discard 后封面引用应级联消失")
	}
	for _, name := range []string{f.BlobName, coverBlob} {
		if _, err := os.Stat(transfer.OutboxArtifactPath(name)); !os.IsNotExist(err) {
			t.Fatalf("discard 后产物应删除: %s", name)
		}
	}
}

// TestE2EGCTwoNamespaces gc 两命名空间分账:trash 按命名空间路由删除,
// 封面孤儿单独报告且不自动删。
func TestE2EGCTwoNamespaces(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	jpg := makeJPEG(t, t.TempDir(), "待删.jpg", 200, 200)

	n, _ := e.m.UploadPaths(ctx, []string{jpg}, 1)
	if n != 1 {
		t.Fatalf("入队: %d", n)
	}
	allDone(t, waitIdle(t, e.m))
	hits, _ := e.db.Search("待删.jpg", 5)
	f, _ := e.db.GetFile(hits[0].ID)

	if err := e.db.SoftDeleteFiles([]int64{f.ID}); err != nil {
		t.Fatal(err)
	}
	coverNames, _ := e.db.CoverBlobNamesOf([]int64{f.ID})
	if err := e.db.MarkBlobTrash(append([]string{f.BlobName}, coverNames...)); err != nil {
		t.Fatal(err)
	}

	// dry-run:列出 2 个待删(文件+封面),不动远端
	mainBefore, _ := e.store.ListBlobs(ctx)
	coversBefore, _ := e.store.ListCoverBlobs(ctx)
	res, err := transfer.RunGC(ctx, e.store, e.db, true)
	if err != nil || len(res.Deleted) != 2 {
		t.Fatalf("dry-run 待删应含文件与封面: %+v %v", res, err)
	}
	mainAfter, _ := e.store.ListBlobs(ctx)
	coversAfter, _ := e.store.ListCoverBlobs(ctx)
	if len(mainAfter) != len(mainBefore) || len(coversAfter) != len(coversBefore) {
		t.Fatal("dry-run 不应动远端")
	}

	// 实删:两个命名空间的对象都没了
	res, err = transfer.RunGC(ctx, e.store, e.db, false)
	if err != nil || len(res.Deleted) != 2 {
		t.Fatalf("实删: %+v %v", res, err)
	}
	if covers, _ := e.store.ListCoverBlobs(ctx); len(covers) != 0 {
		t.Fatalf("covers 命名空间应清空: %+v", covers)
	}

	// 封面孤儿:只报告,不删
	orphan := filepath.Join(t.TempDir(), "orphan-cover")
	if err := os.WriteFile(orphan, []byte("no-owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	of, _ := os.Open(orphan)
	if err := e.store.PutCoverBlob(ctx, "11112222333344445555666677778888", of, nil); err != nil {
		t.Fatal(err)
	}
	of.Close()
	res, err = transfer.RunGC(ctx, e.store, e.db, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CoverOrphans) != 1 || len(res.Orphans) != 0 {
		t.Fatalf("封面孤儿应单独分组: %+v", res)
	}
	if covers, _ := e.store.ListCoverBlobs(ctx); len(covers) != 1 {
		t.Fatal("封面孤儿不应被自动删除")
	}
}

// TestE2EMigrateLegacyCovers 存量缩略图出库:逐行 raw 事务不计 revision、
// 断点续跑、字节一致、软删遗留清除、VACUUM 回收库空间。
func TestE2EMigrateLegacyCovers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// 常规直传一张图:正常走 covers 出库,同时人为补一行 legacy(模拟
	// "迁移后又出现的遗留")——它已有 covers 引用,迁移应收尾清掉
	jpg := makeJPEG(t, t.TempDir(), "常规.jpg", 300, 300)
	if n, _ := e.m.UploadPaths(ctx, []string{jpg}, 1); n != 1 {
		t.Fatal("入队失败")
	}
	allDone(t, waitIdle(t, e.m))
	hits, _ := e.db.Search("常规.jpg", 5)
	if _, err := e.db.Exec(
		`INSERT INTO thumbnails (file_id, data, width, height, mime) VALUES (?,?,?,?,?)`,
		hits[0].ID, make([]byte, 200<<10), 8, 8, "image/jpeg"); err != nil {
		t.Fatal(err)
	}

	// 两个待迁移文件:各塞 200KB 假缩略图(足以观察 VACUUM 前后的库体积)
	seed := func(name string, blob string) (int64, []byte) {
		p := makeFile(t, t.TempDir(), name, 50)
		if n, _ := e.m.UploadPaths(ctx, []string{p}, 1); n != 1 {
			t.Fatal("入队失败")
		}
		allDone(t, waitIdle(t, e.m))
		hs, _ := e.db.Search(name, 5)
		id := hs[0].ID
		data := make([]byte, 200<<10)
		for i := range data {
			data[i] = byte(i * 7)
		}
		if _, err := e.db.Exec(
			`INSERT INTO thumbnails (file_id, data, width, height, mime) VALUES (?,?,?,?,?)`,
			id, data, 100, 100, "image/jpeg"); err != nil {
			t.Fatal(err)
		}
		_ = blob
		return id, data
	}
	idA, dataA := seed("旧图A.txt", "covA")
	_, _ = seed("旧图B.txt", "covB")

	// 软删一个带 legacy 行的文件:它的行应被清除而非迁移
	seed("软删.txt", "covC")
	hs, _ := e.db.Search("软删.txt", 5)
	deadID := hs[0].ID
	if err := e.db.SoftDeleteFiles([]int64{deadID}); err != nil {
		t.Fatal(err)
	}

	dbSizeBefore := dbFootprint(t, e.db.Path)
	rev0, _ := e.db.Revision()

	// 第一轮 --max 1:只迁移 1 行
	res, err := transfer.MigrateLegacyCovers(ctx, e.store, e.db, e.mk, false, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Migrated != 1 || res.Total != 2 {
		t.Fatalf("第一轮(--max 1): %+v err=%v", res, err)
	}
	// 第二轮跑完
	res, err = transfer.MigrateLegacyCovers(ctx, e.store, e.db, e.mk, false, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Migrated != 1 || res.Failed != 0 {
		t.Fatalf("第二轮: %+v err=%v", res, err)
	}
	rev1, _ := e.db.Revision()
	if rev1 != rev0+2 {
		t.Fatalf("两轮迁移应恰 +2 revision(逐行不计): %d → %d", rev0, rev1)
	}

	// 引用与登记:custom/ready + blobs kind=cover
	covA, err := e.db.GetReadyCover(idA)
	if err != nil || covA.Source != index.CoverCustom || covA.Size < int64(len(dataA)) {
		t.Fatalf("迁移行引用(密文大小 ≥ 明文): %+v %v", covA, err)
	}
	states, _ := e.db.ListBlobRecords()
	found := false
	for _, r := range states {
		if r.Name == covA.BlobName {
			found = r.Kind == "cover" && r.State == "active"
		}
	}
	if !found {
		t.Fatal("迁移行应有 kind=cover/active 的 blobs 登记")
	}

	// 字节一致:解密回 200KB 原数据
	got, err := e.fetchAndDecryptCover(t, covA)
	if err != nil || len(got) != len(dataA) {
		t.Fatalf("迁移字节: %d %v", len(got), err)
	}
	for i := range got {
		if got[i] != dataA[i] {
			t.Fatalf("迁移字节不一致 @%d", i)
		}
	}

	// legacy 表清空;软删遗留计入 Deleted
	var left int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM thumbnails`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("legacy 表应清空: %d %v", left, err)
	}

	// 库占用应收窄(VACUUM + checkpoint;WAL 下写入先落 -wal,比总占用)
	dbSizeAfter := dbFootprint(t, e.db.Path)
	if dbSizeAfter >= dbSizeBefore {
		t.Fatalf("VACUUM 后库占用应变小: %d → %d", dbSizeBefore, dbSizeAfter)
	}

	// 重跑:全部跳过,revision 不再动
	rev2, _ := e.db.Revision()
	res, err = transfer.MigrateLegacyCovers(ctx, e.store, e.db, e.mk, false, 0, nil)
	if err != nil || res.Migrated != 0 || res.Total != 0 {
		t.Fatalf("重跑应无事可做: %+v %v", res, err)
	}
	rev3, _ := e.db.Revision()
	if rev3 != rev2 {
		t.Fatalf("重跑不得再计 revision: %d → %d", rev2, rev3)
	}
}

// dbFootprint 统计 SQLite 库文件族(主文件 + WAL + SHM)的总占用:
// WAL 模式下写入先落 -wal,只看主文件会得出"库只有 4KB"的错觉。
func dbFootprint(t *testing.T, dbPath string) int64 {
	t.Helper()
	total := int64(0)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if st, err := os.Stat(dbPath + suffix); err == nil {
			total += st.Size()
		}
	}
	return total
}

// TestE2EMigrateDryRun dry-run 只统计,不动数据。
func TestE2EMigrateDryRun(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := makeFile(t, t.TempDir(), "统计.txt", 30)
	if n, _ := e.m.UploadPaths(ctx, []string{p}, 1); n != 1 {
		t.Fatal("入队失败")
	}
	allDone(t, waitIdle(t, e.m))
	hs, _ := e.db.Search("统计.txt", 5)
	if _, err := e.db.Exec(
		`INSERT INTO thumbnails (file_id, data, width, height, mime) VALUES (?,?,?,?,?)`,
		hs[0].ID, []byte("12345"), 1, 1, "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	rev0, _ := e.db.Revision()
	res, err := transfer.MigrateLegacyCovers(ctx, e.store, e.db, e.mk, true, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || res.Bytes != 5 {
		t.Fatalf("dry-run 统计: %+v", res)
	}
	rev1, _ := e.db.Revision()
	if rev1 != rev0 {
		t.Fatal("dry-run 不得计 revision")
	}
	var left int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM thumbnails`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("dry-run 不得删 legacy 行: %d %v", left, err)
	}
}
