package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAprMD5(t *testing.T) {
	// vector generated with: openssl passwd -apr1 -salt saltsalt secret123
	want := "$apr1$saltsalt$AVDPWMuMvN31vdabhs8HK1"
	if got := aprMD5("secret123", "saltsalt"); got != want {
		t.Errorf("aprMD5 = %q, want %q", got, want)
	}
}

func TestVerify(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "htpasswd")
	content := `# comment
plainuser:plainpass
aproner:$apr1$saltsalt$AVDPWMuMvN31vdabhs8HK1
shauser:{SHA}5en6G6MezRroT3XKqkdPOmY/BfQ=
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		user, pass string
		want       bool
	}{
		{"plainuser", "plainpass", true},
		{"plainuser", "wrong", false},
		{"aproner", "secret123", true},
		{"aproner", "secret124", false},
		{"shauser", "secret", true}, // {SHA} of "secret"
		{"shauser", "other", false},
		{"nosuch", "x", false},
	}
	for _, c := range cases {
		if got := f.Verify(c.user, c.pass); got != c.want {
			t.Errorf("Verify(%q, %q) = %v, want %v", c.user, c.pass, got, c.want)
		}
	}
}
