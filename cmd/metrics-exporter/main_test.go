package main

import "testing"

func TestPrometheusQuoteEscapesLabels(t *testing.T) {
	got := prometheusQuote("a\"b\\c\n")
	want := `"a\"b\\c\n"`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
