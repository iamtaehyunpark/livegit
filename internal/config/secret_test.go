package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPasswordRoundTrip(t *testing.T) {
	t.Setenv("LG_HOME", t.TempDir())

	// No file yet -> empty, no error.
	if pw, err := LoadPassword(); err != nil || pw != "" {
		t.Fatalf("expected empty/no-error before save, got %q err=%v", pw, err)
	}

	const secret = "s3cr3t p@ss / with spaces"
	if err := SavePassword(secret); err != nil {
		t.Fatal(err)
	}
	// Stored file must not contain the plaintext and must be 0600.
	raw, err := os.ReadFile(CredentialsPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == secret || containsSub(raw, secret) {
		t.Fatal("credentials file contains the plaintext password")
	}
	if info, _ := os.Stat(CredentialsPath()); info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file mode = %v, want 0600", info.Mode().Perm())
	}

	got, err := LoadPassword()
	if err != nil || got != secret {
		t.Fatalf("round-trip: got %q err=%v, want %q", got, err, secret)
	}
}

func TestPasswordWrongMachineFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LG_HOME", home)
	if err := SavePassword("hunter2"); err != nil {
		t.Fatal(err)
	}
	// Simulate a different machine by corrupting the ciphertext (a different key
	// would likewise fail GCM authentication).
	p := CredentialsPath()
	b, _ := os.ReadFile(p)
	b[len(b)/2] ^= 0xff
	_ = os.WriteFile(filepath.Clean(p), b, 0o600)
	if _, err := LoadPassword(); err == nil {
		t.Fatal("expected decryption to fail on tampered/other-machine ciphertext")
	}
}

func containsSub(hay []byte, needle string) bool {
	return len(needle) > 0 && len(hay) >= len(needle) &&
		(string(hay) == needle || indexOf(string(hay), needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
