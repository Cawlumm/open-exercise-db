package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesystemStore_PutGetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFilesystemStore(dir)
	if err != nil {
		t.Fatalf("NewFilesystemStore: %v", err)
	}

	ctx := context.Background()
	want := []byte("fake jpeg bytes")

	if err := store.Put(ctx, "exercises/abc/0.jpg", bytes.NewReader(want), "image/jpeg"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := store.Get(ctx, "exercises/abc/0.jpg")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("round-tripped bytes = %q, want %q", got, want)
	}

	if _, err := os.Stat(filepath.Join(dir, "exercises", "abc", "0.jpg")); err != nil {
		t.Errorf("expected file on disk at the resolved path: %v", err)
	}
}

func TestFilesystemStore_Put_OverwritesCleanly(t *testing.T) {
	store, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystemStore: %v", err)
	}
	ctx := context.Background()

	if err := store.Put(ctx, "x.jpg", bytes.NewReader([]byte("first")), "image/jpeg"); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if err := store.Put(ctx, "x.jpg", bytes.NewReader([]byte("second")), "image/jpeg"); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	rc, err := store.Get(ctx, "x.jpg")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "second" {
		t.Errorf("Get after overwrite = %q, want %q", got, "second")
	}
}

func TestFilesystemStore_Get_MissingKey(t *testing.T) {
	store, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystemStore: %v", err)
	}

	if _, err := store.Get(context.Background(), "does/not/exist.jpg"); err == nil {
		t.Fatal("Get: expected error for missing key, got nil")
	}
}

func TestFilesystemStore_RejectsPathTraversal(t *testing.T) {
	store, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystemStore: %v", err)
	}
	ctx := context.Background()

	tests := []string{"", "../escape.jpg", "a/../../escape.jpg"}
	for _, key := range tests {
		if err := store.Put(ctx, key, bytes.NewReader([]byte("x")), "image/jpeg"); err == nil {
			t.Errorf("Put(%q): expected error, got nil", key)
		}
		if _, err := store.Get(ctx, key); err == nil {
			t.Errorf("Get(%q): expected error, got nil", key)
		}
	}
}

func TestFilesystemStore_NewCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "media")
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("precondition: %s should not exist yet", dir)
	}

	if _, err := NewFilesystemStore(dir); err != nil {
		t.Fatalf("NewFilesystemStore: %v", err)
	}

	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("expected %s to exist as a directory after NewFilesystemStore", dir)
	}
}
