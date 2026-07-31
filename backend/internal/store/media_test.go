package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestMediaStore_InsertAndGetByStorageKey(t *testing.T) {
	s, _ := newTestStore(t)

	width, height := 800, 600
	license := "Unlicense"
	id, err := s.Media.InsertImage(context.Background(), InsertImageInput{
		StorageKey:  "exercises/Bench_Press/0.jpg",
		ContentType: "image/jpeg",
		Width:       &width,
		Height:      &height,
		License:     &license,
		Status:      "approved",
	})
	if err != nil {
		t.Fatalf("InsertImage: %v", err)
	}
	if id == "" {
		t.Fatal("InsertImage returned an empty id")
	}

	img, err := s.Media.GetImageByStorageKey(context.Background(), "exercises/Bench_Press/0.jpg")
	if err != nil {
		t.Fatalf("GetImageByStorageKey: %v", err)
	}
	if img.ID != id {
		t.Errorf("ID = %s, want %s", img.ID, id)
	}
	if img.ContentType != "image/jpeg" {
		t.Errorf("ContentType = %q, want image/jpeg", img.ContentType)
	}
	if img.Width == nil || *img.Width != 800 {
		t.Errorf("Width = %v, want 800", img.Width)
	}
	if img.Status != "approved" {
		t.Errorf("Status = %q, want approved", img.Status)
	}
	if img.UploadedBy != nil {
		t.Errorf("UploadedBy = %v, want nil (trusted import has no submitting user)", img.UploadedBy)
	}
}

func TestMediaStore_GetImageByStorageKey_NotFound(t *testing.T) {
	s, _ := newTestStore(t)

	_, err := s.Media.GetImageByStorageKey(context.Background(), "does/not/exist.jpg")
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("err = %v, want pgx.ErrNoRows", err)
	}
}
