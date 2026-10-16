package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type storedAccount struct {
	Account
	Secret string `json:"cookie"`
}

func atomicPrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".accounts-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (s *Server) accountCipher(create bool) (cipher.AEAD, error) {
	path := filepath.Join(s.data, "accounts.key")
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && create {
		// Never replace a lost key when encrypted account data already exists.
		if _, e := os.Stat(filepath.Join(s.data, "accounts.enc")); e == nil || !errors.Is(e, os.ErrNotExist) {
			return nil, errors.New("account encryption key missing")
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		if err = atomicPrivate(path, key); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("invalid account encryption key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Caller holds mu. Cookie is included only in this private encrypted payload.
func (s *Server) saveAccounts() error {
	items := make([]storedAccount, len(s.accounts))
	for i, a := range s.accounts {
		items[i] = storedAccount{Account: a, Secret: a.Cookie}
	}
	plain, err := json.Marshal(items)
	if err != nil {
		return err
	}
	if len(plain) > 4*1024*1024-64 {
		return errors.New("account store too large")
	}
	aead, err := s.accountCipher(true)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	data := append([]byte{1}, nonce...)
	data = aead.Seal(data, nonce, plain, []byte("hark2api-accounts-v1"))
	return atomicPrivate(filepath.Join(s.data, "accounts.enc"), data)
}

func (s *Server) loadAccounts() error {
	path := filepath.Join(s.data, "accounts.enc")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("account store unreadable")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4*1024*1024+1))
	f.Close()
	if err != nil || len(data) > 4*1024*1024 {
		return errors.New("account store invalid size")
	}
	aead, err := s.accountCipher(false)
	if err != nil {
		return errors.New("account encryption key unreadable or missing")
	}
	n := aead.NonceSize()
	if len(data) < 1+n+aead.Overhead() || data[0] != 1 {
		return errors.New("account store invalid format")
	}
	plain, err := aead.Open(nil, data[1:1+n], data[1+n:], []byte("hark2api-accounts-v1"))
	if err != nil {
		return errors.New("account store decryption failed")
	}
	var items []storedAccount
	if json.Unmarshal(plain, &items) != nil {
		return errors.New("account store invalid payload")
	}
	seen := map[string]bool{}
	for _, item := range items {
		if item.ID == "" || seen[item.ID] || strings.TrimSpace(item.Name) == "" || len([]rune(item.Name)) > 40 || len(item.Secret) > 16000 || !strings.Contains(item.Secret, "=") || strings.ContainsAny(item.Secret, "\r\n") {
			return errors.New("account store invalid record")
		}
		seen[item.ID] = true
		item.Account.Cookie = item.Secret
		s.accounts = append(s.accounts, item.Account)
	}
	return nil
}
