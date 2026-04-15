package db

import "testing"

func TestExtractHost(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "postgres URL",
			dsn:  "postgres://user:pass@localhost:5432/mydb",
			want: "localhost",
		},
		{
			name: "postgres URL with host only",
			dsn:  "postgres://myhost/mydb",
			want: "myhost",
		},
		{
			name: "empty DSN",
			dsn:  "",
			want: "unknown",
		},
		{
			name: "invalid URL",
			dsn:  "://bad",
			want: "unknown",
		},
		{
			name: "key=value DSN (no host in URL form)",
			dsn:  "host=myserver user=foo dbname=bar",
			want: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractHost(tt.dsn)
			if got != tt.want {
				t.Errorf("extractHost(%q) = %q, want %q", tt.dsn, got, tt.want)
			}
		})
	}
}
