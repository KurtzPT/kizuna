package main

import (
	"net"
	"strings"
	"testing"
)

func TestListenPortInUse(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	_, err = listen(taken.Addr().String())
	if err == nil {
		t.Fatal("second listen on the same port succeeded")
	}
	_, port, _ := net.SplitHostPort(taken.Addr().String())
	if msg := err.Error(); !strings.Contains(msg, "port "+port+" is already in use") || !strings.Contains(msg, "PORT") {
		t.Errorf("unclear error: %s", msg)
	}

	ln, err := listen("127.0.0.1:0") // a free port works
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
}
