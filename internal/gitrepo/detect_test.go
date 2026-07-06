package gitrepo

import "testing"

func TestDetectKindFromNames(t *testing.T) {
	cases := []struct {
		name      string
		files     []string
		wantKind  string
		wantCompo string
	}{
		{"dockerfile wins", []string{"Dockerfile", "compose.yaml", "main.go"}, "dockerfile", ""},
		{"compose when no dockerfile", []string{"compose.yaml", "app.py"}, "compose", "compose.yaml"},
		{"docker-compose.yml", []string{"docker-compose.yml"}, "compose", "docker-compose.yml"},
		{"compose precedence order", []string{"docker-compose.yaml", "compose.yml"}, "compose", "compose.yml"},
		{"nixpacks fallback", []string{"main.go", "go.mod"}, "nixpacks", ""},
		{"empty", nil, "nixpacks", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, compose := detectKindFromNames(tc.files)
			if kind != tc.wantKind || compose != tc.wantCompo {
				t.Fatalf("detectKindFromNames(%v) = %q,%q; want %q,%q",
					tc.files, kind, compose, tc.wantKind, tc.wantCompo)
			}
		})
	}
}
