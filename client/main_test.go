package main

import "testing"

func TestHello(t *testing.T) {
	got := Hello()
	if got != "hello" {
		t.Errorf("Hello() = %q, want %q", got, "hello")
	}
}
