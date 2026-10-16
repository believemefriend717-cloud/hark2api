package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAccountStoreRejectsTamperingAndMissingKey(t *testing.T) {
	for _, lostKey := range []bool{false, true} {
		s := testServer(t)
		assertStatus(t, call(s, "POST", "/admin/accounts", `{"name":"fixture","cookie":"session=private-fixture"}`, ""), 201)
		if lostKey {
			if err := os.Remove(filepath.Join(s.data, "accounts.key")); err != nil {
				t.Fatal(err)
			}
		} else {
			path := filepath.Join(s.data, "accounts.enc")
			b, _ := os.ReadFile(path)
			b[len(b)-1] ^= 1
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := New(s.data); err == nil {
			t.Fatal("corrupt store accepted")
		}
		if lostKey {
			if _, err := os.Stat(filepath.Join(s.data, "accounts.key")); !os.IsNotExist(err) {
				t.Fatal("lost key replaced")
			}
		}
	}
}

func TestAccountImportSaveFailureDoesNotClaimSuccess(t *testing.T) {
	s := testServer(t)
	if err := os.Mkdir(filepath.Join(s.data, "accounts.enc"), 0700); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, call(s, "POST", "/admin/accounts", `{"name":"fixture","cookie":"session=private-fixture"}`, ""), 500)
	if len(s.accounts) != 0 {
		t.Fatal("failed import remained in memory")
	}
}

func TestAccountStatusSurvivesRestart(t *testing.T) {
	s := testServer(t)
	assertStatus(t, call(s, "POST", "/admin/accounts", `{"name":"fixture","cookie":"session=private-fixture"}`, ""), 201)
	s.mu.Lock()
	s.accounts[0].Status = "valid"
	s.accounts[0].Checked = "2026-10-09T00:00:00Z"
	s.accounts[0].ChatVerified = true
	s.accounts[0].ChatChecked = "2026-10-09T00:00:01Z"
	err := s.saveAccounts()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	after, err := New(s.data)
	if err != nil || len(after.accounts) != 1 || after.accounts[0].Status != "valid" || after.accounts[0].Checked == "" || !after.accounts[0].ChatVerified || after.accounts[0].ChatChecked == "" {
		t.Fatal("status not restored", err)
	}
}
