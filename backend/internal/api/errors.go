package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// Errors in this API are RFC 9457 Problem Details, which huma emits natively:
// `application/problem+json` carrying title/status/detail, plus a per-field
// `errors` array on validation failures.
//
// What huma does not supply is a *stable* identifier. `detail` is prose
// written for a human ("exercise not found"), and prose is exactly what gets
// reworded later; a client branching on it breaks silently the first time
// someone improves the wording. Kubernetes solved the same problem by
// carrying Status.Reason (machine-readable) beside Status.Message (human)
// beside Status.Code (numeric HTTP), and RFC 9457 reserves `type` — a URI —
// for exactly this. So every error this API returns sets `type`, and that
// URI is what consumers are documented to switch on.
//
// The URIs are dereferenceable under the project's own domain rather than
// opaque slugs like "exercise_not_found": RFC 9457 §3.1.1 expects a client to
// be able to follow the type URI to human-readable documentation, and a
// string in a JSON body that leads nowhere gives that up for no saving.

// errorTypeBase prefixes every problem type URI. A constant rather than
// something derived from the request host: the URI identifies the *class of
// problem*, not the server reporting it, so a self-hosted instance and the
// public one must emit identical values. Deriving it from the host would
// mean a client that switches on the URI stops working the moment it points
// at a different deployment.
const errorTypeBase = "https://openexercisedb.org/errors/"

// Problem types — one per stable, documented failure mode. New failure modes
// get a constant here rather than an inline string at a call site, so the
// complete set stays enumerable from one place; that list is what the public
// error documentation is written from.
const (
	// ErrTypeNotFound is any addressable resource that does not exist.
	ErrTypeNotFound = errorTypeBase + "not-found"
	// ErrTypeValidation is a malformed or out-of-range request parameter.
	// Raised by huma's own validation before a handler ever runs, which is
	// the main reason the hook below exists rather than per-call-site
	// stamping.
	ErrTypeValidation = errorTypeBase + "validation-failed"
	// ErrTypeRateLimited is a request rejected by internal/ratelimit.
	ErrTypeRateLimited = errorTypeBase + "rate-limited"
	// ErrTypeInternal is a server-side failure. Carries no detail beyond
	// "internal error" on purpose — see mapStoreError in api.go.
	ErrTypeInternal = errorTypeBase + "internal"
	// ErrTypeMethodNotAllowed is a known path addressed with a verb it does
	// not serve. Every operation in Phase 1 is a GET, so this is what a write
	// attempt against the read API gets — a distinct condition from
	// ErrTypeNotFound, and one a client can act on differently.
	ErrTypeMethodNotAllowed = errorTypeBase + "method-not-allowed"
)

// problemTypeForStatus maps an HTTP status onto its problem type.
//
// Status-derived rather than call-site-supplied because the errors most
// likely to be missed are the ones this package never constructs: huma's
// request validation (422), its unknown-method handling (405), and anything
// a future middleware raises. Deriving from status covers those by
// construction. A per-call-site scheme covers only what someone remembered
// to annotate, and a client cannot distinguish "this API doesn't use type
// URIs" from "this one error forgot one" — which is worse than not having
// the field at all.
func problemTypeForStatus(status int) string {
	switch {
	case status == http.StatusNotFound:
		return ErrTypeNotFound
	case status == http.StatusMethodNotAllowed:
		return ErrTypeMethodNotAllowed
	case status == http.StatusTooManyRequests:
		return ErrTypeRateLimited
	case status == http.StatusUnprocessableEntity, status == http.StatusBadRequest:
		return ErrTypeValidation
	case status >= 500:
		return ErrTypeInternal
	default:
		return ""
	}
}

// init overrides huma.NewError so every error huma constructs — this
// package's huma.Error404NotFound calls, huma's internal request-validation
// failures, and anything a future middleware raises — carries a problem type.
//
// huma.NewError is a package-level var whose own doc comment presents
// reassignment as the supported way to customize error responses. It is
// global state, which is worth naming plainly: this process serves exactly
// one API, so there is no second consumer of huma to surprise.
//
// Set in init rather than in Register specifically so tests get identical
// behavior. Several tests in this package call registerExerciseRoutes and
// friends directly on a throwaway humatest.API without going through
// Register (see api.go's note on route ordering for why that pattern
// exists); anything wired inside Register would be absent there, and the
// tests would then be asserting against a differently-behaved API than the
// one that ships.
func init() {
	inner := huma.NewError
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		err := inner(status, msg, errs...)
		model, ok := err.(*huma.ErrorModel)
		if !ok {
			// A caller replaced NewError with something returning a
			// different concrete type. Pass it through untouched rather
			// than dropping the error to force a type in.
			return err
		}
		// "about:blank" is RFC 9457's explicit no-type-information value and
		// huma's schema default, so treat it as unset rather than as a
		// deliberate choice worth preserving.
		if model.Type == "" || model.Type == "about:blank" {
			model.Type = problemTypeForStatus(status)
		}
		return model
	}
}

// Routing-level errors — a typo'd path, a write attempt against a read-only
// operation — are raised by net/http's ServeMux, not by huma, so the hook
// above never sees them. Left alone they answer `text/plain` with
// "404 page not found" or "Method Not Allowed\n": the only responses in this
// API that are neither JSON nor problem+json, reached by exactly the mistakes
// a first-time integrator makes.
//
// The obvious fix — mount a catch-all handler on the bare "/" pattern — is
// wrong, and was shipped and reverted here. Go's ServeMux only enters its 405
// branch when *no* node matches the request path; registering "/" guarantees a
// match, so registering a catch-all silently deletes 405 handling for the
// whole server and answers every wrong-verb request with a 404 and no `Allow`
// header, telling an integrator the resource does not exist when the path was
// right and only the method was wrong. Verified directly against this Go
// version. Nor can the catch-all be narrowed to specific methods: `HEAD /`
// panics at registration ("matches fewer methods than GET /api/v1/exercises,
// but has a more general path pattern"), so there is no route-shaped fix at
// all.
//
// Hence a wrapper rather than a route. It leaves routing untouched — ServeMux
// still picks 404 vs. 405 and still sets `Allow` — and only rewrites the
// response body on the way out.

// problemBodies holds the rewritten body for each status this wrapper
// handles, built once at init rather than per request.
var problemBodies = map[int][]byte{
	http.StatusNotFound: []byte(`{"type":"` + ErrTypeNotFound +
		`","title":"Not Found","status":404,` +
		`"detail":"no route matches this path; see /docs for the available operations"}`),
	http.StatusMethodNotAllowed: []byte(`{"type":"` + ErrTypeMethodNotAllowed +
		`","title":"Method Not Allowed","status":405,` +
		`"detail":"this path does not serve that method; see the Allow header, and /docs for the available operations"}`),
}

// problemInterceptor rewrites ServeMux's plain-text routing errors into
// problem+json, passing everything else through untouched.
//
// The discriminator is the Content-Type already on the response when
// WriteHeader is called: net/http's http.Error sets `text/plain; charset=utf-8`
// before writing the status, while every handler in this package answers
// `application/problem+json`. That keeps the rewrite scoped to responses the
// router generated and leaves a genuine 404 from a huma handler — which is
// already in the right shape, with its own operation-specific detail —
// completely alone.
type problemInterceptor struct {
	http.ResponseWriter
	rewrote bool
	handled bool
}

func (w *problemInterceptor) WriteHeader(status int) {
	if w.handled {
		return
	}
	w.handled = true

	body, isRoutingStatus := problemBodies[status]
	if !isRoutingStatus || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		w.ResponseWriter.WriteHeader(status)
		return
	}

	w.rewrote = true
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	// http.Error sets nosniff alongside its text/plain body. Harmless to keep,
	// but it describes a body that is no longer being sent, so drop it with the
	// content type it belonged to.
	w.Header().Del("X-Content-Type-Options")
	w.ResponseWriter.WriteHeader(status)
	_, _ = w.ResponseWriter.Write(body)
}

func (w *problemInterceptor) Write(b []byte) (int, error) {
	if !w.handled {
		w.WriteHeader(http.StatusOK)
	}
	if w.rewrote {
		// Swallow ServeMux's plain-text body; the replacement is already
		// written. Report full length so the caller sees a successful write
		// rather than a short-write error it would only log noise about.
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap exposes the wrapped writer to http.ResponseController, which is how
// Go 1.20+ reaches a real writer's optional interfaces (Flusher, Hijacker,
// deadline setters) through a middleware chain. Without it this wrapper would
// silently strip every one of those capabilities from everything mounted
// behind it — the kind of middleware bug that stays invisible until something
// downstream needs to flush.
func (w *problemInterceptor) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ProblemErrors wraps h so routing-level 404s and 405s answer in problem+json
// like every other error this API returns. Mounted in cmd/api-server/main.go
// around the whole mux.
func ProblemErrors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&problemInterceptor{ResponseWriter: w}, r)
	})
}
