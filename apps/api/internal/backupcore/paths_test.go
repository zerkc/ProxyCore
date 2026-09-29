package backupcore

import "testing"

func TestEscapesBundleRoot(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		escapes bool
	}{
		{name: "empty", path: "", escapes: true},
		{name: "absolute", path: "/absolute/path", escapes: true},
		{name: "parent", path: "../outside.json", escapes: true},
		{name: "nested parent", path: "db/../outside.json", escapes: true},
		{name: "multiple parents", path: "certs/../../outside.key", escapes: true},
		{name: "backslash", path: `certs\site.key`, escapes: true},
		{name: "windows drive", path: "C:/x", escapes: true},
		{name: "windows drive lower case", path: "c:/x", escapes: true},
		{name: "windows drive backslash", path: `C:\x`, escapes: true},
		{name: "nested windows drive", path: "cert/C:/x.json", escapes: true},
		{name: "relative", path: "relative/path.json", escapes: false},
		{name: "nested relative", path: "db/nested/users.json", escapes: false},
		{name: "dot segment", path: "./relative/path.json", escapes: false},
		{name: "drive relative", path: "C:relative", escapes: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EscapesBundleRoot(tt.path); got != tt.escapes {
				t.Fatalf("EscapesBundleRoot(%q) = %v, want %v", tt.path, got, tt.escapes)
			}
		})
	}
}
