// Package credentials keeps the openetl API token (and the saved base URL /
// project) in one AES-256-GCM encrypted file. The key lives in the OS keyring
// and falls back to a 0600 key file when no keyring is available.
package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zalando/go-keyring"
)

const (
	fileName    = "credentials.enc"
	keyFileName = ".encryption_key"
	service     = "openetl"
	keyUser     = "encryption-key"
	version     = byte(1)
)

// ErrNotFound means nothing has been saved yet.
var ErrNotFound = errors.New("no saved credentials")

// Credentials is the plaintext content of the encrypted file.
type Credentials struct {
	BaseURL string `json:"base_url,omitempty"`
	Project string `json:"project,omitempty"`
	Token   string `json:"token"`
}

// Keyring abstracts the OS keyring so tests never touch a real one.
type Keyring interface {
	Get(service, user string) (string, error)
	Set(service, user, secret string) error
}

// OSKeyring is the zalando/go-keyring backed implementation.
type OSKeyring struct{}

func (OSKeyring) Get(s, u string) (string, error) { return keyring.Get(s, u) }
func (OSKeyring) Set(s, u, p string) error        { return keyring.Set(s, u, p) }

// Store reads and writes the encrypted credential file in one directory.
type Store struct {
	dir string
	kr  Keyring
}

// DefaultDir is ~/.config/openetl under the given home directory.
func DefaultDir(home string) string { return filepath.Join(home, ".config", "openetl") }

// New returns a store rooted at dir. A nil keyring means the OS keyring.
func New(dir string, kr Keyring) *Store {
	if kr == nil {
		kr = OSKeyring{}
	}
	return &Store{dir: dir, kr: kr}
}

// Path is the location of the encrypted file.
func (s *Store) Path() string { return filepath.Join(s.dir, fileName) }

func (s *Store) keyPath() string { return filepath.Join(s.dir, keyFileName) }

// Load decrypts the saved credentials; ErrNotFound when there are none.
func (s *Store) Load() (*Credentials, error) {
	raw, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	key, _, err := s.key(false)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(raw) < 1+gcm.NonceSize() || raw[0] != version {
		return nil, errors.New("credentials file is corrupt; run `openetl auth login` again")
	}
	n := gcm.NonceSize()
	plain, err := gcm.Open(nil, raw[1:1+n], raw[1+n:], nil)
	if err != nil {
		return nil, errors.New("cannot decrypt credentials (key changed?); run `openetl auth login` again")
	}
	var c Credentials
	if err := json.Unmarshal(plain, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save encrypts and writes the credentials (mode 0600, atomic replace).
func (s *Store) Save(c Credentials) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	key, _, err := s.key(true)
	if err != nil {
		return err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return err
	}
	out := append([]byte{version}, nonce...)
	out = gcm.Seal(out, nonce, plain, nil)
	return writeFile(s.Path(), out)
}

// Delete removes the credential file (the key is kept). Missing is not an error.
func (s *Store) Delete() error {
	err := os.Remove(s.Path())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// KeySource reports where the key currently lives: "keyring", "file" or "none".
func (s *Store) KeySource() string {
	if _, src, err := s.key(false); err == nil {
		return src
	}
	return "none"
}

func newGCM(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (s *Store) keyringGet() (string, error) {
	type res struct {
		v   string
		err error
	}
	ch := make(chan res, 1)
	go func() { v, err := s.kr.Get(service, keyUser); ch <- res{v, err} }()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-time.After(2 * time.Second):
		return "", errors.New("keyring timeout")
	}
}

func decodeKey(h string) ([]byte, error) {
	k, err := hex.DecodeString(h)
	if err != nil || len(k) != 32 {
		return nil, errors.New("invalid encryption key")
	}
	return k, nil
}

// key returns the encryption key and where it came from. With create, a new
// key is generated (keyring first, key file as the fallback) when none exists.
func (s *Store) key(create bool) ([]byte, string, error) {
	if h, err := s.keyringGet(); err == nil {
		if k, err := decodeKey(h); err == nil {
			return k, "keyring", nil
		}
	}
	if b, err := os.ReadFile(s.keyPath()); err == nil {
		if k, err := decodeKey(string(trimNL(b))); err == nil {
			return k, "file", nil
		}
	}
	if !create {
		return nil, "", errors.New("encryption key not found; run `openetl auth login` again")
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, "", err
	}
	h := hex.EncodeToString(k)
	if err := s.kr.Set(service, keyUser, h); err == nil {
		return k, "keyring", nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, "", err
	}
	if err := writeFile(s.keyPath(), []byte(h+"\n")); err != nil {
		return nil, "", fmt.Errorf("store encryption key: %w", err)
	}
	return k, "file", nil
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}
