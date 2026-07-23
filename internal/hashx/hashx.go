// Package hashx provides the one content-hash function used on both sides so
// Ghost and Source always compute the same identity for a file's bytes
// (used for conflict detection and sync-point tracking).
package hashx

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
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

// New returns a streaming hasher producing the same identity as Bytes/File;
// finish with Sum.
func New() hash.Hash { return sha256.New() }

// Sum finalizes a hasher from New into the canonical hex form.
func Sum(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }
