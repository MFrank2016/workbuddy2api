package main

import "testing"

func TestIsLoopbackListen(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{":7863", false},
		{"0.0.0.0:7863", false},
		{"127.0.0.1:7863", true},
		{"[::1]:7863", true},
		{"localhost:7863", true},
		{"192.168.1.2:7863", false},
	}
	for _, c := range cases {
		if got := isLoopbackListen(c.addr); got != c.want {
			t.Errorf("isLoopbackListen(%q)=%v want %v", c.addr, got, c.want)
		}
	}
}
