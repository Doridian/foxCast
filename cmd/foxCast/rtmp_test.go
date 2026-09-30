package main

import "testing"

func TestListenRTMPIsLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0", "localhost:0"} {
		ln, err := listenRTMP(addr)
		if err != nil {
			t.Errorf("%s: %v", addr, err)
			continue
		}
		_ = ln.Close()
	}
	for _, addr := range []string{"0.0.0.0:0", ":0", "[::]:0", "192.168.1.2:1935", "example.com:1935", "127.0.0.1"} {
		if ln, err := listenRTMP(addr); err == nil {
			_ = ln.Close()
			t.Errorf("%s: listening, want an error", addr)
		}
	}
}
