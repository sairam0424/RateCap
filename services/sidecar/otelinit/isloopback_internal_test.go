package otelinit

import "testing"

func TestIsLoopbackEndpoint(t *testing.T) {
	cases := []struct {
		endpoint string
		want     bool
	}{
		{"127.0.0.1:4317", true},
		{"localhost:4317", true},
		{"[::1]:4317", true},
		{"10.0.0.5:4317", false},
		{"otel-collector.internal.example.com:4317", false},
		{"192.168.1.50:4317", false},
	}
	for _, c := range cases {
		if got := isLoopbackEndpoint(c.endpoint); got != c.want {
			t.Errorf("isLoopbackEndpoint(%q) = %v, want %v", c.endpoint, got, c.want)
		}
	}
}
