package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cawlumm/open-exercise-db/internal/api"
)

func TestVersionDefaultsToDev(t *testing.T) {
	if version != "dev" {
		t.Fatalf("version = %q without -ldflags, want %q", version, "dev")
	}
}

func TestWriteOpenAPI_ReportsInjectedVersion(t *testing.T) {
	var buf bytes.Buffer
	if err := writeOpenAPI(&buf, "9.8.7"); err != nil {
		t.Fatalf("writeOpenAPI: %v", err)
	}

	var doc struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if doc.Info.Version != "9.8.7" {
		t.Errorf("info.version = %q, want 9.8.7", doc.Info.Version)
	}
	if _, ok := doc.Paths["/api/v1/exercises"]; !ok {
		t.Errorf("paths is missing /api/v1/exercises; has %d paths", len(doc.Paths))
	}
}

func TestRegisterAPI_IndexAndSpecAgree(t *testing.T) {
	mux := http.NewServeMux()
	registerAPI(mux, "3.2.1", nil, nil, nil, nil, "", api.AuthConfig{})

	for path, field := range map[string]func(map[string]any) any{
		"/api/v1":              func(m map[string]any) any { return m["version"] },
		"/api/v1/openapi.json": func(m map[string]any) any { return m["info"].(map[string]any)["version"] },
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("%s: %v (status %d)", path, err, rec.Code)
		}
		if got := field(m); got != "3.2.1" {
			t.Errorf("%s reports version %v, want 3.2.1", path, got)
		}
	}
}
