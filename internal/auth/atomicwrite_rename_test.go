package auth

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// withRenameSeam 替换 os.Rename 注入点与 GOOS 判定，并在用例结束后还原
// （两者都是进程级全局，本文件用例不并行）。
func withRenameSeam(t *testing.T, fakeGOOS string, fn func(old, new string) error) {
	t.Helper()
	oldRename, oldGOOS := renameFile, goos
	renameFile, goos = fn, fakeGOOS
	t.Cleanup(func() { renameFile, goos = oldRename, oldGOOS })
}

// TestRenameRetry 覆盖 Windows-only 的原子重命名重试：
// Windows 上杀毒/索引服务会短暂占用目标文件，rename 报 EPERM/EACCES/EBUSY/EMFILE/ENFILE，
// 固定退避 50/100/200/400ms 循环、总上限 2s、最多 20 次尝试；**非 Windows 完全不重试**。
// 注入点让这条 Windows 分支在 Linux CI 上被真实执行（不是 skip、不是 mock 掉逻辑）。
func TestRenameRetry(t *testing.T) {
	t.Run("windows retries EPERM three times then succeeds", func(t *testing.T) {
		dir := t.TempDir()
		fp := filepath.Join(dir, "workbuddy-u1.json")
		seed := `{"auth":{"accessToken":"old","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"u1"}}`
		if err := os.WriteFile(fp, []byte(seed), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(fp, 0o600); err != nil {
			t.Fatal(err)
		}

		attempts := 0
		withRenameSeam(t, "windows", func(old, new string) error {
			attempts++
			if attempts <= 3 {
				return &os.LinkError{Op: "rename", Old: old, New: new, Err: syscall.EPERM}
			}
			return os.Rename(old, new)
		})

		start := time.Now()
		err := writeAtomicWithMode(fp, []byte(`{"auth":{"accessToken":"new"},"account":{"uid":"u1"}}`), "u1")
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("save after 3 EPERM must succeed, got: %v", err)
		}
		if attempts != 4 {
			t.Errorf("rename attempts = %d, want 4 (3 failures + 1 success)", attempts)
		}
		if elapsed > 2*time.Second {
			t.Errorf("elapsed = %v, want <= 2s wall-clock cap", elapsed)
		}
		// EPERM 绝不放宽权限：目标文件必须还是 0600。
		if got := permOf(t, fp); got != 0o600 {
			t.Errorf("mode after retried rename = %04o, want 0600 (never chmod on EPERM)", got)
		}
		raw, err := os.ReadFile(fp)
		if err != nil {
			t.Fatal(err)
		}
		if b, err := Parse(raw); err != nil || b.AccessToken != "new" {
			t.Errorf("content after retry = %s (parse err %v), want accessToken=new", raw, err)
		}
	})

	t.Run("linux never retries", func(t *testing.T) {
		dir := t.TempDir()
		fp := filepath.Join(dir, "workbuddy-u2.json")
		attempts := 0
		withRenameSeam(t, "linux", func(old, new string) error {
			attempts++
			return &os.LinkError{Op: "rename", Old: old, New: new, Err: syscall.EPERM}
		})

		err := writeAtomicWithMode(fp, []byte(`{}`), "u2")
		if err == nil {
			t.Fatal("want rename error on linux, got nil")
		}
		if attempts != 1 {
			t.Errorf("rename attempts on linux = %d, want 1 (retry must be inert off Windows)", attempts)
		}
	})

	t.Run("windows does not retry non-transient errors", func(t *testing.T) {
		dir := t.TempDir()
		fp := filepath.Join(dir, "workbuddy-u3.json")
		attempts := 0
		withRenameSeam(t, "windows", func(old, new string) error {
			attempts++
			return &os.LinkError{Op: "rename", Old: old, New: new, Err: syscall.ENOENT}
		})

		err := writeAtomicWithMode(fp, []byte(`{}`), "u3")
		if err == nil {
			t.Fatal("want rename error, got nil")
		}
		if attempts != 1 {
			t.Errorf("attempts = %d, want 1 (ENOENT is not retryable)", attempts)
		}
		if !errors.Is(err, syscall.ENOENT) {
			t.Errorf("error = %v, want original ENOENT surfaced", err)
		}
	})

	t.Run("windows caps wall clock and attempts", func(t *testing.T) {
		dir := t.TempDir()
		fp := filepath.Join(dir, "workbuddy-u4.json")
		attempts := 0
		withRenameSeam(t, "windows", func(old, new string) error {
			attempts++
			return &os.LinkError{Op: "rename", Old: old, New: new, Err: syscall.EPERM}
		})

		start := time.Now()
		err := writeAtomicWithMode(fp, []byte(`{}`), "u4")
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("want persistent EPERM to surface as error")
		}
		if attempts > 20 {
			t.Errorf("attempts = %d, want <= 20", attempts)
		}
		if elapsed > 2500*time.Millisecond {
			t.Errorf("elapsed = %v, want <= 2s cap (+ scheduling slack)", elapsed)
		}
		if runtime.GOOS == "windows" {
			// Windows 本身也要满足同样的上限；Linux 上该分支已由注入的 GOOS 执行。
			t.Logf("native windows run: attempts=%d elapsed=%v", attempts, elapsed)
		}
	})
}
