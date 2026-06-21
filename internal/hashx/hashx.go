// Package hashx provides the one content-hash function used on both sides so
// Ghost and Source always compute the same identity for a file's bytes
// (used for conflict detection in §4.4 and sync-point tracking in §4.1).
package hashx

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// Bytes returns the hex sha256 of b.
func Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// File returns the hex sha256 of a file's contents. A missing file hashes to "".
func File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
