//go:build !unix

package auth

import "os"

// chownFile 在非 POSIX 平台是空操作（没有 uid/gid 概念）。
var chownFile = func(name string, uid, gid int) error { return nil }

// statOwner 在非 POSIX 平台恒返回 ok=false。
func statOwner(fi os.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
