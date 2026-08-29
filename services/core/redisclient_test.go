package main

import (
	"strings"
	"testing"
)

func TestParseSentinelAddrs_EmptyStringReturnsNil(t *testing.T) {
	got := parseSentinelAddrs("")
	if got != nil {
		t.Errorf("expected nil for empty string, got %v", got)
	}
}

func TestParseSentinelAddrs_SplitsCommaSeparatedList(t *testing.T) {
	got := parseSentinelAddrs("sentinel-0:26379,sentinel-1:26379,sentinel-2:26379")
	want := []string{"sentinel-0:26379", "sentinel-1:26379", "sentinel-2:26379"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestParseSentinelAddrs_TrimsWhitespaceAroundEntries(t *testing.T) {
	got := parseSentinelAddrs(" sentinel-0:26379 , sentinel-1:26379 ")
	want := []string{"sentinel-0:26379", "sentinel-1:26379"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("expected %v, got %v", want, got)
	}
}
