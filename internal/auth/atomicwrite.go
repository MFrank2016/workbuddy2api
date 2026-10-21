package auth

import (
	"log"
	"os"
)

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
	return os.Rename(tmp, path)
}
