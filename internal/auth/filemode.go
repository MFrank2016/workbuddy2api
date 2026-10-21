package auth

import (
	"fmt"
	"os"
	"sync/atomic"
)

// defaultFileMode 新 auth 文件的默认权限，与改动前的 0o600 一致（零行为变化）。
const defaultFileMode os.FileMode = 0o600

// fileMode 新 auth 文件的权限覆盖值（进程级）。
//
// 为什么需要它：auth 文件可能被**跨 uid** 读写 —— manager（面板）写入、core 读取，
// 两边 uid 不同。core 用 0o600 新建文件会让 manager 的 uid 读不到。
// 已有文件的权限在 SaveAtomic 里一律以磁盘现状为准，此覆盖只作用于**新建**文件。
//
// 由 Todo 11 把 config.Auth.FileMode 接到 SetFileMode（本 todo 不加配置字段）。
var fileMode atomic.Uint32

func init() { fileMode.Store(uint32(defaultFileMode)) }

// SetFileMode 设置新 auth 文件的权限，仅接受 0o600 / 0o640 / 0o644。
// 其它值返回错误且不改变当前设置（防止把 auth 文件写成组/全局可写）。
func SetFileMode(mode os.FileMode) error {
	switch mode {
	case 0o600, 0o640, 0o644:
		fileMode.Store(uint32(mode))
		return nil
	default:
		return fmt.Errorf("auth: unsupported file mode %#o (allowed: 0600, 0640, 0644)", mode)
	}
}

// FileMode 返回当前用于新建 auth 文件的权限。
func FileMode() os.FileMode { return os.FileMode(fileMode.Load()) }
