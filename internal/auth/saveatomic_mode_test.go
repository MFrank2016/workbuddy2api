package auth

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// resetFileMode 让每个子用例从「默认 0600」起步（fileMode 是进程级全局）。
func resetFileMode(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if err := SetFileMode(0o600); err != nil {
			t.Fatalf("restore default file mode: %v", err)
		}
	})
}

func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Mode().Perm()
}

// TestSaveAtomicPreservesMode 覆盖跨 uid 修复的核心语义：
// 已有文件的 mode（以及 uid/gid）在 token 刷新写回后保持不变；新文件走默认/显式 mode。
func TestSaveAtomicPreservesMode(t *testing.T) {
	resetFileMode(t)

	t.Run("existing 0644 stays 0644", func(t *testing.T) {
		dir := t.TempDir()
		fp := filepath.Join(dir, "workbuddy-u1.json")
		seed := `{"auth":{"accessToken":"old","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"u1"}}`
		if err := os.WriteFile(fp, []byte(seed), 0o644); err != nil {
			t.Fatal(err)
		}
		// 显式 chmod：WriteFile 的 mode 受 umask 影响，这里要一个确定的 0644 输入。
		if err := os.Chmod(fp, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := permOf(t, fp); got != 0o644 {
			t.Fatalf("fixture mode = %04o, want 0644", got)
		}

		a := &Auth{AccessToken: "new", RefreshToken: "r2", ExpiresAt: 2, UID: "u1", FilePath: fp}
		if err := a.SaveAtomic(); err != nil {
			t.Fatalf("save: %v", err)
		}

		if got := permOf(t, fp); got != 0o644 {
			t.Errorf("mode after refresh = %04o, want 0644 (must not narrow manager-written file)", got)
		}
		// token 刷新语义不变：新 token 必须落盘，且能被 Parse 读回。
		raw, err := os.ReadFile(fp)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Parse(raw)
		if err != nil {
			t.Fatalf("reparse: %v", err)
		}
		if b.AccessToken != "new" || b.RefreshToken != "r2" {
			t.Errorf("tokens after save = %q/%q, want new/r2", b.AccessToken, b.RefreshToken)
		}
	})

	t.Run("existing 0600 stays 0600", func(t *testing.T) {
		dir := t.TempDir()
		fp := filepath.Join(dir, "workbuddy-u2.json")
		seed := `{"auth":{"accessToken":"old","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"u2"}}`
		if err := os.WriteFile(fp, []byte(seed), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(fp, 0o600); err != nil {
			t.Fatal(err)
		}

		a := &Auth{AccessToken: "new", RefreshToken: "r2", ExpiresAt: 2, UID: "u2", FilePath: fp}
		if err := a.SaveAtomic(); err != nil {
			t.Fatalf("save: %v", err)
		}

		if got := permOf(t, fp); got != 0o600 {
			t.Errorf("mode after refresh = %04o, want 0600 (must not widen permissions)", got)
		}
	})

	t.Run("new file defaults to 0600", func(t *testing.T) {
		dir := t.TempDir()
		fp := filepath.Join(dir, "workbuddy-u3.json")
		a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1, UID: "u3", FilePath: fp}
		if err := a.SaveAtomic(); err != nil {
			t.Fatalf("save: %v", err)
		}
		if got := permOf(t, fp); got != 0o600 {
			t.Errorf("new file mode = %04o, want 0600 (zero behavior change)", got)
		}
	})

	t.Run("explicit override 0640 applies to new file", func(t *testing.T) {
		if err := SetFileMode(0o640); err != nil {
			t.Fatalf("SetFileMode(0640): %v", err)
		}
		if got := FileMode(); got != 0o640 {
			t.Fatalf("FileMode() = %04o, want 0640", got)
		}
		dir := t.TempDir()
		fp := filepath.Join(dir, "workbuddy-u4.json")
		a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1, UID: "u4", FilePath: fp}
		if err := a.SaveAtomic(); err != nil {
			t.Fatalf("save: %v", err)
		}
		if got := permOf(t, fp); got != 0o640 {
			t.Errorf("new file mode = %04o, want 0640 (explicit override)", got)
		}
	})

	t.Run("explicit override does not widen existing file", func(t *testing.T) {
		if err := SetFileMode(0o644); err != nil {
			t.Fatalf("SetFileMode(0644): %v", err)
		}
		dir := t.TempDir()
		fp := filepath.Join(dir, "workbuddy-u5.json")
		seed := `{"auth":{"accessToken":"old","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"u5"}}`
		if err := os.WriteFile(fp, []byte(seed), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(fp, 0o600); err != nil {
			t.Fatal(err)
		}
		a := &Auth{AccessToken: "new", RefreshToken: "r2", ExpiresAt: 2, UID: "u5", FilePath: fp}
		if err := a.SaveAtomic(); err != nil {
			t.Fatalf("save: %v", err)
		}
		if got := permOf(t, fp); got != 0o600 {
			t.Errorf("mode after refresh = %04o, want 0600 (existing mode wins over override)", got)
		}
	})

	t.Run("unsupported override is rejected", func(t *testing.T) {
		if err := SetFileMode(0o600); err != nil {
			t.Fatalf("baseline SetFileMode(0600): %v", err)
		}
		if err := SetFileMode(0o666); err == nil {
			t.Error("SetFileMode(0666) should be rejected")
		}
		if got := FileMode(); got != 0o600 {
			t.Errorf("FileMode() after rejected set = %04o, want unchanged 0600", got)
		}
	})

	t.Run("chown EPERM warns and still saves", func(t *testing.T) {
		dir := t.TempDir()
		fp := filepath.Join(dir, "workbuddy-u6.json")
		seed := `{"auth":{"accessToken":"old","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"u6"}}`
		if err := os.WriteFile(fp, []byte(seed), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(fp, 0o644); err != nil {
			t.Fatal(err)
		}

		// 注入 chown 失败：测试环境常是 root，真实 os.Chown 不会 EPERM，
		// 只能通过 seam 走这条分支（不是 skip，分支被真实执行）。
		oldChown := chownFile
		chownFile = func(string, int, int) error { return syscall.EPERM }
		t.Cleanup(func() { chownFile = oldChown })

		// 捕获 log 输出（log.SetOutput 是进程级全局，本用例不并行）。
		oldWriter := log.Writer()
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		log.SetOutput(pw)

		a := &Auth{AccessToken: "new", RefreshToken: "r2", ExpiresAt: 2, UID: "u6", FilePath: fp}
		saveErr := a.SaveAtomic()
		_ = pw.Close()
		raw, _ := io.ReadAll(pr)
		log.SetOutput(oldWriter)

		if saveErr != nil {
			t.Fatalf("save must not fail on chown EPERM, got: %v", saveErr)
		}
		if !strings.Contains(string(raw), "chown") || !strings.Contains(string(raw), "operation not permitted") {
			t.Errorf("want chown EPERM warning in log, got: %s", string(raw))
		}
		// 关键：chown 失败绝不静默降级 mode。
		if got := permOf(t, fp); got != 0o644 {
			t.Errorf("mode after chown EPERM = %04o, want 0644 (never silently downgrade)", got)
		}
	})
}

// TestSaveAtomicPreservesOwner 验证 root 环境下 uid/gid 真被保留（跨 uid 修复）。
// 非 root 无法 chown 夹具，此用例退化为只断言 mode 保留（不做 skip，仍执行保存路径）。
func TestSaveAtomicPreservesOwner(t *testing.T) {
	resetFileMode(t)
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u7.json")
	seed := `{"auth":{"accessToken":"old","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"u7"}}`
	if err := os.WriteFile(fp, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	asRoot := os.Geteuid() == 0
	if asRoot {
		if err := os.Chown(fp, 12345, 12345); err != nil {
			t.Fatalf("chown fixture: %v", err)
		}
	}

	a := &Auth{AccessToken: "new", RefreshToken: "r2", ExpiresAt: 2, UID: "u7", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}

	if got := permOf(t, fp); got != 0o644 {
		t.Errorf("mode = %04o, want 0644", got)
	}
	if !asRoot {
		return
	}
	st, err := os.Stat(fp)
	if err != nil {
		t.Fatal(err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("unexpected stat type %T", st.Sys())
	}
	if int(sys.Uid) != 12345 || int(sys.Gid) != 12345 {
		t.Errorf("owner after save = %d:%d, want 12345:12345", sys.Uid, sys.Gid)
	}
}
