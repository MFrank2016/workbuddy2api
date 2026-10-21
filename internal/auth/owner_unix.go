//go:build unix

package auth

import (
	"os"
	"syscall"
)

// chownFile 是 os.Chown 的注入点：测试环境通常是 root，真实 os.Chown 不会 EPERM，
// 需要 seam 才能真实执行「chown 失败只 warning」这条分支。
var chownFile = func(name string, uid, gid int) error { return os.Chown(name, uid, gid) }

// statOwner 取出文件的 uid/gid；无法取得（非 POSIX stat）时 ok=false。
func statOwner(fi os.FileInfo) (uid, gid int, ok bool) {
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
