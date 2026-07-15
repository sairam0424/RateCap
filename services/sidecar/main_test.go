package main

import "testing"

func TestResolveMaxInflight_EmptyStringReturnsDefault(t *testing.T) {
	got := resolveMaxInflight("", 500)
	if got != 500 {
		t.Errorf("expected 500 for empty string, got %d", got)
	}
}

func TestResolveMaxInflight_ValidPositiveValueIsUsed(t *testing.T) {
	got := resolveMaxInflight("3", 500)
	if got != 3 {
		t.Errorf("expected 3, got %d", got)
	}
}

func TestResolveMaxInflight_UnparseableStringReturnsDefault(t *testing.T) {
	got := resolveMaxInflight("not-a-number", 500)
	if got != 500 {
		t.Errorf("expected 500 for an unparseable value, got %d", got)
	}
}

func TestResolveMaxInflight_ZeroReturnsDefault(t *testing.T) {
	got := resolveMaxInflight("0", 500)
	if got != 500 {
		t.Errorf("expected 500 for a zero value (would shed every request), got %d", got)
	}
}

func TestResolveMaxInflight_NegativeReturnsDefault(t *testing.T) {
	got := resolveMaxInflight("-5", 500)
	if got != 500 {
		t.Errorf("expected 500 for a negative value, got %d", got)
	}
}
