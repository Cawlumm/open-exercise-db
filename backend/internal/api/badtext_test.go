package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
)

// The middleware has to turn input Postgres cannot store into a 422 before any
// handler or query runs, so this needs no database: a stub handler stands in.
func TestRejectBadTextMiddleware(t *testing.T) {
	_, api := humatest.New(t)
	api.UseMiddleware(rejectBadTextMiddleware(api))
	huma.Get(api, "/echo", func(ctx context.Context, in *struct {
		Q string `query:"q"`
	}) (*struct{ Body string }, error) {
		return &struct{ Body string }{Body: in.Q}, nil
	})

	for _, tc := range []struct {
		name, query string
		want        int
	}{
		{"plain", "q=squat", http.StatusOK},
		{"utf-8 accented", "q=extensi%C3%B3n", http.StatusOK},
		{"emoji", "q=%F0%9F%92%AA", http.StatusOK},
		{"no query", "", http.StatusOK},
		{"latin-1 byte in a value", "q=extensi%F3n", http.StatusUnprocessableEntity},
		{"truncated multi-byte sequence", "q=%C3", http.StatusUnprocessableEntity},
		{"invalid byte in a key", "%F3=1", http.StatusUnprocessableEntity},
		{"NUL byte", "q=a%00b", http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/echo"
			if tc.query != "" {
				path += "?" + tc.query
			}
			if got := api.Get(path).Code; got != tc.want {
				t.Errorf("GET %s = %d, want %d", path, got, tc.want)
			}
		})
	}
}
