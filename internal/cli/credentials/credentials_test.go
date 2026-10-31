package credentials

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/zalando/go-keyring"
)

type failKR struct{}

func (failKR) Get(string, string) (string, error) { return "", errors.New("no keyring") }
func (failKR) Set(string, string, string) error   { return errors.New("no keyring") }

type memKR struct{ m map[string]string }

func (k *memKR) Get(s, u string) (string, error) {
	if v, ok := k.m[s+u]; ok {
		return v, nil
	}
	return "", keyring.ErrNotFound
}
func (k *memKR) Set(s, u, p string) error { k.m[s+u] = p; return nil }

func TestFileKeyFallbackRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, failKR{})
	if _, err := s.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	want := Credentials{BaseURL: "https://x", Project: "acme", Token: "secret-token-123"}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.Path(), s.keyPath()} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", p, st.Mode().Perm())
		}
	}
	raw, _ := os.ReadFile(s.Path())
	if bytes.Contains(raw, []byte("secret-token-123")) {
		t.Fatal("token stored in plaintext")
	}
	got, err := New(dir, failKR{}).Load()
	if err != nil || *got != want {
		t.Fatalf("got %+v err %v", got, err)
	}
	if src := s.KeySource(); src != "file" {
		t.Errorf("key source %q", src)
	}
	if err := s.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatal("expected not found after delete")
	}
}

func TestKeyringPreferred(t *testing.T) {
	dir := t.TempDir()
	kr := &memKR{m: map[string]string{}}
	s := New(dir, kr)
	if err := s.Save(Credentials{Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.keyPath()); err == nil {
		t.Fatal("key file written although keyring works")
	}
	if len(kr.m) != 1 {
		t.Fatal("key not in keyring")
	}
	if c, err := New(dir, kr).Load(); err != nil || c.Token != "t" {
		t.Fatalf("%v %v", c, err)
	}
}

func TestOSKeyringMock(t *testing.T) {
	keyring.MockInit()
	s := New(t.TempDir(), nil)
	if err := s.Save(Credentials{Token: "m"}); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Load(); err != nil || c.Token != "m" {
		t.Fatalf("%v %v", c, err)
	}
}

func TestRealSandboxFallback(t *testing.T) {
	// With a keyring that errors, behaviour must match the file fallback.
	keyring.MockInitWithError(errors.New("boom"))
	defer keyring.MockInit()
	s := New(t.TempDir(), nil)
	if err := s.Save(Credentials{Token: "f"}); err != nil {
		t.Fatal(err)
	}
	if s.KeySource() != "file" {
		t.Fatalf("source %s", s.KeySource())
	}
}

func TestTamperDetected(t *testing.T) {
	s := New(t.TempDir(), failKR{})
	_ = s.Save(Credentials{Token: "t"})
	raw, _ := os.ReadFile(s.Path())
	raw[len(raw)-1] ^= 0xff
	_ = os.WriteFile(s.Path(), raw, 0o600)
	if _, err := s.Load(); err == nil {
		t.Fatal("expected decrypt error")
	}
}
