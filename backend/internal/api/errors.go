package api

import (
	"net/http"

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

// notFoundHandler serves RFC 9457 Problem Details for a request that matched
// no registered route.
//
// Without it these fall through to net/http's own mux, whose 404 is
// `text/plain` with the body "404 page not found" — the one response in this
// API that is neither JSON nor problem+json. A consumer's error handling is
// written once against the documented shape, and a typo'd path is a common
// way to first reach this API, so the endpoint most likely to be hit by
// mistake was the one that didn't answer in the documented format.
//
// Registered on the mux in cmd/api-server/main.go rather than through huma:
// huma registers concrete operations and has no wildcard-fallback concept,
// so the catch-all has to sit at the router level, below every real route.
func notFoundHandler() http.Handler {
	body := []byte(`{"type":"` + ErrTypeNotFound +
		`","title":"Not Found","status":404,` +
		`"detail":"no route matches this path; see /docs for the available operations"}`)

	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(body)
	})
}

// NotFoundHandler is notFoundHandler exported for cmd/api-server to mount.
// Kept as a function rather than a package var so the body is built once per
// process at mount time and can never be mutated by a caller holding the
// slice.
func NotFoundHandler() http.Handler { return notFoundHandler() }
