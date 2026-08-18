package crypto

import (
	"bytes"
	"errors"
	"testing"
)

// weakParams 测试专用弱 Argon2 参数,避免每个用例都付出 64MiB 派生成本。
func weakParams() Argon2Params { return Argon2Params{Time: 1, MemoryKiB: 1024, Threads: 1} }

func TestKeyFileRoundTrip(t *testing.T) {
	kf, mk, err := CreateKeyFile("正确口令", weakParams())
	if err != nil {
		t.Fatalf("创建 keyfile: %v", err)
	}
	if len(kf.Bytes()) != keyFileFixedSize {
		t.Fatalf("keyfile 长度 = %d,期望 %d", len(kf.Bytes()), keyFileFixedSize)
	}
	parsed, err := ParseKeyFile(kf.Bytes())
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	got, err := parsed.Unlock("正确口令")
	if err != nil {
		t.Fatalf("解锁: %v", err)
	}
	if got != mk {
		t.Fatal("往返后主密钥不一致")
	}
}

func TestKeyFileWrongPassphrase(t *testing.T) {
	kf, _, err := CreateKeyFile("对的", weakParams())
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := ParseKeyFile(kf.Bytes())
	if _, err := parsed.Unlock("错的"); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("期望 ErrBadPassphrase,得到 %v", err)
	}
}

// TestKeyFileTamper 逐个位置翻转字节:解析期或解锁期必须至少有一处失败。
func TestKeyFileTamper(t *testing.T) {
	kf, _, err := CreateKeyFile("口令", weakParams())
	if err != nil {
		t.Fatal(err)
	}
	good := kf.Bytes()
	// 覆盖:magic/盐/派生参数/wrapNonce/密文与 tag 区
	offsets := []int{0, 5, 12, 20, 30, 34, 36, 38, 50, 62, 70, 100, 109}
	for _, off := range offsets {
		bad := bytes.Clone(good)
		bad[off] ^= 0xFF
		parsed, err := ParseKeyFile(bad)
		if err != nil {
			continue // magic/版本等静态字段被篡改,解析期即失败,符合预期
		}
		if _, err := parsed.Unlock("口令"); err == nil {
			t.Fatalf("偏移 %d 被篡改后解锁仍然成功", off)
		}
	}
}

func TestKeyFileBadFormat(t *testing.T) {
	kf, _, err := CreateKeyFile("p", weakParams())
	if err != nil {
		t.Fatal(err)
	}
	good := kf.Bytes()
	badMagic := append([]byte("X"), good[1:]...)
	badVersion := bytes.Clone(good)
	badVersion[8] = 9
	badKeyLen := bytes.Clone(good)
	badKeyLen[37] = 33
	cases := [][]byte{good[:109], badMagic, badVersion, badKeyLen}
	for i, c := range cases {
		if _, err := ParseKeyFile(c); err == nil {
			t.Fatalf("非法 keyfile(用例 %d)不应解析成功", i)
		} else if !errors.Is(err, ErrBadKeyFile) {
			t.Fatalf("用例 %d:期望 ErrBadKeyFile,得到 %v", i, err)
		}
	}
}

func TestKeyFileRewrap(t *testing.T) {
	kf, mk, err := CreateKeyFile("旧口令", weakParams())
	if err != nil {
		t.Fatal(err)
	}
	before := bytes.Clone(kf.Bytes())
	if err := kf.Rewrap("旧口令", "新口令"); err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	if bytes.Equal(before, kf.Bytes()) {
		t.Fatal("Rewrap 后序列化结果不应不变")
	}
	parsed, _ := ParseKeyFile(kf.Bytes())
	if _, err := parsed.Unlock("旧口令"); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("旧口令应失效,得到 %v", err)
	}
	got, err := parsed.Unlock("新口令")
	if err != nil {
		t.Fatalf("新口令解锁: %v", err)
	}
	if got != mk {
		t.Fatal("Rewrap 不应改变主密钥本身")
	}
}
