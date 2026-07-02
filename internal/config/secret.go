package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Password auth for a Source is stored encrypted at rest (never plaintext, never
// in config.yaml). The key is derived from a machine identifier, so copying the
// credentials file to another machine yields an undecryptable blob. This is the
// "good enough" personal-tool bar from the design, not a hardened secret
// store.

// appSalt separates lg's derived key from any other use of the machine id.
const appSalt = "lg-credential-v1"

// CredentialsPath is the per-project encrypted credentials file.
func CredentialsPath() string { return filepath.Join(Dir(), "credentials") }

var platformUUID = regexp.MustCompile(`IOPlatformUUID"\s*=\s*"([^"]+)"`)

// machineID returns a stable per-machine identifier.
func machineID() string {
	// macOS: the hardware UUID.
	if out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output(); err == nil {
		if m := platformUUID.FindSubmatch(out); m != nil {
			return string(m[1])
		}
	}
	// Linux: the dbus/systemd machine id.
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil {
			if id := strings.TrimSpace(string(b)); id != "" {
				return id
			}
		}
	}
	// Fallback: hostname + home (weak, but better than a constant).
	host, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	return host + "|" + home
}

func machineKey() [32]byte {
	return sha256.Sum256([]byte(machineID() + "|" + appSalt))
}

// SavePassword encrypts pw with the machine key and writes it (0600) to the
// project's credentials file.
func SavePassword(pw string) error {
	key := machineKey()
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(pw), nil)
	if err := os.MkdirAll(filepath.Dir(CredentialsPath()), 0o755); err != nil {
		return err
	}
	enc := base64.StdEncoding.EncodeToString(sealed)
	return os.WriteFile(CredentialsPath(), []byte(enc+"\n"), 0o600)
}

// LoadPassword decrypts the stored password, or returns "" if none is stored.
func LoadPassword() (string, error) {
	raw, err := os.ReadFile(CredentialsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	sealed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return "", fmt.Errorf("credentials file is corrupt: %w", err)
	}
	key := machineKey()
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(sealed) < gcm.NonceSize() {
		return "", fmt.Errorf("credentials file is truncated")
	}
	nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	pw, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("cannot decrypt credentials (moved from another machine?): %w", err)
	}
	return string(pw), nil
}
