package store

import "testing"

func TestIsBinaryContentType(t *testing.T) {
	tests := []struct {
		ct   string
		want bool
	}{
		{"", false},
		{"text/plain", false},
		{"text/plain; charset=utf-8", false},
		{"image/png", true},
		{"application/octet-stream", true},
	}
	for _, tt := range tests {
		got := isBinaryContentType(tt.ct)
		if got != tt.want {
			t.Errorf("isBinaryContentType(%q) = %v, want %v", tt.ct, got, tt.want)
		}
	}
}
