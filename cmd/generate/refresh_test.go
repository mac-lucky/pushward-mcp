package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRefreshAsset_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fresh content"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "asset.txt")
	refreshAsset(false, srv.URL, dest)

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading asset: %v", err)
	}
	if string(got) != "fresh content" {
		t.Errorf("asset = %q, want %q", got, "fresh content")
	}
}

func TestRefreshAsset_UseLocalIsNoOp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server must not be called when useLocal is set")
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "asset.txt")
	if err := os.WriteFile(dest, []byte("committed"), 0o644); err != nil {
		t.Fatal(err)
	}
	refreshAsset(true, srv.URL, dest)

	got, _ := os.ReadFile(dest)
	if string(got) != "committed" {
		t.Errorf("useLocal should keep committed asset, got %q", got)
	}
}

func TestRefreshAsset_FetchFailureKeepsCommitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "asset.txt")
	if err := os.WriteFile(dest, []byte("committed"), 0o644); err != nil {
		t.Fatal(err)
	}
	refreshAsset(false, srv.URL, dest)

	got, _ := os.ReadFile(dest)
	if string(got) != "committed" {
		t.Errorf("fetch failure must leave committed asset untouched, got %q", got)
	}
}
