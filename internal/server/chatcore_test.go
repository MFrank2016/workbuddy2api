package server

import "testing"

func TestChatExecOptionsZeroValue(t *testing.T) {
	var opt ChatExecOptions
	if opt.Protocol != "" {
		t.Fatal("zero value protocol must be empty")
	}
	ex := &ChatExecution{}
	if ex.Release != nil {
		t.Fatal("release must be nil until set")
	}
}
