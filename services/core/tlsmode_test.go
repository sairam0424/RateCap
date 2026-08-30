package main

import "testing"

func TestResolveTLSMode_EmptyStringDefaultsToOff(t *testing.T) {
	got, err := resolveTLSMode("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "off" {
		t.Errorf(`expected "off", got %q`, got)
	}
}

func TestResolveTLSMode_ExplicitOffIsAccepted(t *testing.T) {
	got, err := resolveTLSMode("off")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "off" {
		t.Errorf(`expected "off", got %q`, got)
	}
}

func TestResolveTLSMode_PermissiveIsAccepted(t *testing.T) {
	got, err := resolveTLSMode("permissive")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "permissive" {
		t.Errorf(`expected "permissive", got %q`, got)
	}
}

func TestResolveTLSMode_StrictIsAccepted(t *testing.T) {
	got, err := resolveTLSMode("strict")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "strict" {
		t.Errorf(`expected "strict", got %q`, got)
	}
}

func TestResolveTLSMode_InvalidValueReturnsError(t *testing.T) {
	_, err := resolveTLSMode("YOLO")
	if err == nil {
		t.Fatal("expected an error for an invalid RATECAP_TLS_MODE value")
	}
}
