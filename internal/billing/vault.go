// Package billing fetches normalized monthly infrastructure cost from VPS
// provider billing APIs and stores per-server provider credentials, sealed at
// rest. It is the infra-spend half of the cross-fleet cost dashboard (issue
// #60); the AI-spend half lives in package cost.
package billing

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

const keyBytes = 32

// Vault seals provider API keys with AES-256-GCM. Unlike the github/cliauth
// token vaults — which write ciphertext to on-disk blobs and keep only a path
// in SQLite — this vault returns the ciphertext and nonce to the caller, which
// stores them inline in the provider_credentials table. The only off-store
// secret is the AES key file, which is held to 0600 / root-owned (matching the
// other vaults) so a leaked database is not a leaked credential.
type Vault struct {
	KeyPath          string
	RequireRootOwner bool
}

// NewVault constructs a vault keyed by the file at keyPath. The key is created
// on first use.
func NewVault(keyPath string) *Vault {
	return &Vault{KeyPath: keyPath, RequireRootOwner: os.Geteuid() == 0}
}

// Seal encrypts plaintext, returning the ciphertext and the nonce it was sealed
// under. provider is bound in as additional authenticated data so a credential
// row cannot be transplanted between providers.
func (v *Vault) Seal(provider, plaintext string) (ciphertext, nonce []byte, err error) {
	if provider == "" || plaintext == "" {
		return nil, nil, errors.New("provider and api key required")
	}
	gcm, err := v.gcm()
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	ciphertext = gcm.Seal(nil, nonce, []byte(plaintext), []byte(provider))
	return ciphertext, nonce, nil
}

// Open decrypts a sealed credential back to plaintext.
func (v *Vault) Open(provider string, ciphertext, nonce []byte) (string, error) {
	gcm, err := v.gcm()
	if err != nil {
		return "", err
	}
	if len(nonce) != gcm.NonceSize() {
		return "", errors.New("provider credential nonce is malformed")
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, []byte(provider))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (v *Vault) gcm() (cipher.AEAD, error) {
	key, err := v.key()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (v *Vault) key() ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(v.KeyPath), 0o700); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(v.KeyPath)
	if errors.Is(err, os.ErrNotExist) {
		raw = make([]byte, keyBytes)
		if _, err := io.ReadFull(rand.Reader, raw); err != nil {
			return nil, err
		}
		if err := os.WriteFile(v.KeyPath, raw, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(raw) != keyBytes {
		return nil, fmt.Errorf("billing key must be %d bytes", keyBytes)
	}
	if err := v.validateKeyFile(); err != nil {
		return nil, err
	}
	return raw, nil
}

func (v *Vault) validateKeyFile() error {
	info, err := os.Stat(v.KeyPath)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("billing key permissions must be 0600, got %03o", info.Mode().Perm())
	}
	if v.RequireRootOwner && runtime.GOOS != "windows" {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("cannot inspect billing key owner")
		}
		if st.Uid != 0 {
			return fmt.Errorf("billing key must be root-owned, got uid %d", st.Uid)
		}
	}
	return nil
}
