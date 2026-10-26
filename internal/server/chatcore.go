package server

import (
	"io"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

type ChatExecOptions struct {
	Body     []byte
	Stream   bool
	Protocol string
	OnError  func(status int, code, msg, hint string)
}

type ChatExecution struct {
	RC         io.ReadCloser
	Account    *auth.Auth
	Release    func()
	Status     int
	Aggregated map[string]any
	BareModel  string
	Realm      string
	ModelRate  string
	ChatMeta   upstream.ChatMeta
	Stat       *chatStat
}
