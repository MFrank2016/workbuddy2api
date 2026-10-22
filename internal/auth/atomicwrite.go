package auth

import (
	"errors"
	"log"
	"os"
	"runtime"
	"syscall"
	"time"
)

// renameFile 是 os.Rename 的注入点：Windows 上的重试分支必须在 Linux CI 上
// 可被真实执行（而不是 skip 或只测退避函数）。
var renameFile = os.Rename

// goos 是 runtime.GOOS 的注入点，同样只为让「仅 Windows 重试」这条判定可测。
var goos = runtime.GOOS

const (
	// renameRetryBudget 是重命名重试的总墙钟上限。
	renameRetryBudget = 2 * time.Second
	// renameRetryMaxAttempts 是含首次在内的最大尝试次数（防止退避序列无限拉长）。
	renameRetryMaxAttempts = 20
)

// renameBackoff 是固定退避序列（50/100/200/400ms 循环）。
// 刻意不用「80×50ms」：那会跑到 4s，突破 2s 的墙钟上限。
var renameBackoff = [...]time.Duration{
	50 * time.Millisecond,
	100 * time.Millisecond,
	200 * time.Millisecond,
	400 * time.Millisecond,
}

// isRetryableRenameErr 报告错误是否属于 Windows 上「文件被短暂占用」的瞬时错误。
// 只认这五个 errno：EPERM/EACCES（杀毒软件或索引服务持有句柄）、EBUSY、
// EMFILE/ENFILE（句柄耗尽）。其它错误（ENOENT、跨设备 rename 等）重试无意义。
func isRetryableRenameErr(err error) bool {
	for _, target := range []error{
		syscall.EPERM, syscall.EACCES, syscall.EBUSY, syscall.EMFILE, syscall.ENFILE,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// renameWithRetry 执行 os.Rename，仅在 Windows 上对瞬时占用错误重试。
//
// 为什么只在 Windows：POSIX 的 rename 是同目录内的原子 inode 替换，不会出现
// 「目标被占用」；Linux 上重试只会把真实错误（权限、只读挂载）延迟 2s 才暴露。
//
// ★ 绝不在 EPERM 时放宽权限 ★：把目标 chmod 成 0666 看似能绕过占用，实际是把
// auth 文件（明文 token）变成全局可读可写。宁可保存失败让用户看到错误。
func renameWithRetry(oldpath, newpath string) error {
	err := renameFile(oldpath, newpath)
	if err == nil || goos != "windows" || !isRetryableRenameErr(err) {
		return err
	}
	deadline := time.Now().Add(renameRetryBudget)
	for attempt := 1; attempt < renameRetryMaxAttempts; attempt++ {
		delay := renameBackoff[(attempt-1)%len(renameBackoff)]
		// 睡过截止时间就不睡：宁可少一次尝试，也不能突破 2s 墙钟上限。
		if time.Now().Add(delay).After(deadline) {
			break
		}
		time.Sleep(delay)
		if err = renameFile(oldpath, newpath); err == nil {
			return nil
		}
		if !isRetryableRenameErr(err) {
			return err
		}
	}
	return err
}

// writeAtomicWithMode 把 raw 原子写入 path（tmp + rename），并保持**现有文件**的
// 权限与属主；path 不存在时用 FileMode()（默认 0600）建新文件。
//
// 为什么必须继承磁盘现状：auth 文件可能由 manager（面板，另一 uid）写入、由本进程
// （core）刷新写回。tmp+rename 会替换 inode，若新 inode 不显式继承 mode/owner，
// manager 写的 0644 会在一次 token 刷新后变成 0600，manager 的 uid 从此读不到该账号
// （跨 uid 修复的核心）。
//
// chown 失败（典型 EPERM：非 root 无法把文件交给别的 uid）只 warning 不报错，但
// **绝不因此降级 mode**：读权限的损失比属主不完美严重得多。
//
// FileMode() 由 Todo 11 从 config.Auth.FileMode 接入（本函数不读配置）。
func writeAtomicWithMode(path string, raw []byte, label string) error {
	wantMode := FileMode()
	wantUID, wantGID, haveOwner := -1, -1, false
	if fi, err := os.Stat(path); err == nil {
		wantMode = fi.Mode().Perm()
		wantUID, wantGID, haveOwner = statOwner(fi)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, wantMode); err != nil {
		return err
	}
	// WriteFile 的 mode 受 umask 影响，显式 chmod 才得到确定结果。
	if err := os.Chmod(tmp, wantMode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if haveOwner {
		if err := chownFile(tmp, wantUID, wantGID); err != nil {
			log.Printf("WARN: auth %s chown %d:%d on save: %v（保留 mode %04o，不降级）",
				label, wantUID, wantGID, err, wantMode)
		}
	}
	if err := renameWithRetry(tmp, path); err != nil {
		// 保留 tmp 会让下次写入覆盖它，但更糟的是把「保存失败」变成静默的
		// 「旧文件还在」——调用方看到 error 才能记录/告警，这里只清掉残留。
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
