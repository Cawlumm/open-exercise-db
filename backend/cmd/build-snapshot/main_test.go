package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeAPI() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/meta/locales":
			fmt.Fprint(w, `{"locales":[{"code":"en"}],"total":1}`)
		case "/api/v1/exercises/export":
			fmt.Fprint(w, `{"exercises":[{"id":"a","status":"published"}],"total":1}`)
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestRunFlagsAndExitCodes(t *testing.T) {
	var so, se bytes.Buffer
	if got := run(context.Background(), nil, &so, &se); got != 2 || !strings.Contains(se.String(), "-out") {
		t.Fatalf("missing -out: %d %q", got, se.String())
	}
	if got := run(context.Background(), []string{"-bogus"}, &so, &se); got != 2 {
		t.Fatalf("bad flag: %d", got)
	}

	srv := fakeAPI()
	defer srv.Close()
	out := t.TempDir()
	so.Reset()
	if got := run(context.Background(), []string{"-api", srv.URL, "-out", out}, &so, &se); got != 0 || !strings.HasPrefix(so.String(), "changed") {
		t.Fatalf("first run: %d %q", got, so.String())
	}
	if _, err := os.Stat(filepath.Join(out, "v1", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	so.Reset()
	if got := run(context.Background(), []string{"-api", srv.URL, "-out", out}, &so, &se); got != 0 || so.String() != "unchanged\n" {
		t.Fatalf("second run: %d %q", got, so.String())
	}

	se.Reset()
	if got := run(context.Background(), []string{"-api", srv.URL + "/nope", "-out", t.TempDir()}, &so, &se); got != 1 || !strings.Contains(se.String(), "refusing to publish") {
		t.Fatalf("failure: %d %q", got, se.String())
	}
}
