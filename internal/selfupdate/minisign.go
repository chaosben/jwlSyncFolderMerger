package selfupdate

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// minisign formats (https://jedisct1.github.io/minisign/):
//
//	public key: base64("Ed" | key id (8) | ed25519 public key (32))
//	signature:  untrusted comment line
//	            base64(algorithm "Ed" or "ED" | key id (8) | signature (64))
//	            "trusted comment: " line
//	            base64(signature over signature (64) | trusted comment)
//
// "ED" signs the BLAKE2b-512 hash of the file (the default), "Ed" the file itself.

type publicKey struct {
	id  [8]byte
	key ed25519.PublicKey
}

func parsePublicKey(s string) (publicKey, error) {
	s = strings.TrimSpace(s)
	if lines := strings.Split(s, "\n"); len(lines) == 2 && strings.HasPrefix(lines[0], "untrusted comment:") {
		s = strings.TrimSpace(lines[1])
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return publicKey{}, fmt.Errorf("public key: %w", err)
	}
	if len(raw) != 2+8+ed25519.PublicKeySize || string(raw[:2]) != "Ed" {
		return publicKey{}, errors.New("public key: not a minisign Ed25519 key")
	}
	var pk publicKey
	copy(pk.id[:], raw[2:10])
	pk.key = ed25519.PublicKey(raw[10:])
	return pk, nil
}

// verifyMinisign checks sig against msg with any of the given public keys and
// returns the trusted comment.
func verifyMinisign(keys []string, msg, sig []byte) (string, error) {
	lines := strings.Split(strings.ReplaceAll(string(sig), "\r\n", "\n"), "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[0], "untrusted comment:") {
		return "", errors.New("signature: malformed")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil || len(raw) != 2+8+ed25519.SignatureSize {
		return "", errors.New("signature: malformed")
	}
	alg, id, sigBytes := string(raw[:2]), raw[2:10], raw[10:]
	const tcPrefix = "trusted comment: "
	if !strings.HasPrefix(lines[2], tcPrefix) {
		return "", errors.New("signature: trusted comment missing")
	}
	trusted := strings.TrimSuffix(lines[2][len(tcPrefix):], "\r")
	global, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[3]))
	if err != nil || len(global) != ed25519.SignatureSize {
		return "", errors.New("signature: malformed global signature")
	}

	signed := msg
	switch alg {
	case "ED":
		h := blake2b.Sum512(msg)
		signed = h[:]
	case "Ed":
	default:
		return "", fmt.Errorf("signature: unsupported algorithm %q", alg)
	}
	for _, k := range keys {
		pk, err := parsePublicKey(k)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(pk.id[:], id) {
			continue
		}
		if !ed25519.Verify(pk.key, signed, sigBytes) {
			return "", errors.New("signature: verification failed")
		}
		if !ed25519.Verify(pk.key, append(append([]byte{}, sigBytes...), trusted...), global) {
			return "", errors.New("signature: trusted comment verification failed")
		}
		return trusted, nil
	}
	return "", errors.New("signature: made with an unknown key")
}
