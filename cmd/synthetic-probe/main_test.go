package main

import (
	"bytes"
	"testing"
)

func TestMakeFixtureDeterministic(t *testing.T) {
	a := makeFixture(4096)
	b := makeFixture(4096)
	if len(a) != 4096 || !bytes.Equal(a, b) {
		t.Fatal("fixture must be deterministic and exact-sized")
	}
	wantPNG := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if !bytes.Equal(a[:len(wantPNG)], wantPNG) {
		t.Fatal("fixture does not begin with a PNG signature")
	}
}

func TestLoopbackOnly(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost", "::1"} {
		if !isLoopbackHost(host) {
			t.Fatalf("expected %q to be loopback", host)
		}
	}
	for _, host := range []string{"example.com", "10.0.0.2", "127.0.0.2"} {
		if isLoopbackHost(host) {
			t.Fatalf("did not expect %q to be accepted", host)
		}
	}
}
