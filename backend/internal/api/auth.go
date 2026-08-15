package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/Cawlumm/open-exercise-db/internal/auth"
	"github.com/Cawlumm/open-exercise-db/internal/model"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// authRequiredKey is the huma.Operation.Metadata key an operation sets to
// mark itself as requiring a credential, mirroring rateLimitTierKey's tagging
// scheme in ratelimit.go.
//
// The tag is how an operation opts *in*, but it is not the only thing that
// makes a credential required — see authRequiredFor. A safe default cannot
// come from the tag alone in either direction: defaulting every untagged
// operation to "auth required" would take the public read API offline the
// first time someone forgot a tag, and defaulting to "public" would leave a
// forgotten write world-writable. Neither is acceptable, so the default is
// derived from the HTTP method instead, which no one can forget to set.
const authRequiredKey = "oedb-auth-required"

// safeMethods are the HTTP methods that never modify state, per RFC 9110 §9.2.1.
// An operation using one of these is public unless it tags itself otherwise;
// anything else requires a credential whether it remembered to say so or not.
var safeMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
}

// authRequiredFor decides whether an operation needs a credential.
//
// An explicit tag wins, so a read operation can opt into requiring one. With
// no tag, a non-safe method requires auth by construction — this is the
// fail-closed default that makes forgetting the tag on a future
// POST /submissions a 401 rather than an open write endpoint. It is logged
// as well as enforced, because relying on the default is a mistake even when
// it happens to be safe: the tag documents intent at the registration site,
// and an operation reaching here untagged means nobody stated one.
func authRequiredFor(op *huma.Operation) bool {
	if tagged, ok := op.Metadata[authRequiredKey].(bool); ok {
		return tagged
	}
	if safeMethods[op.Method] {
		return false
	}
	log.Printf("auth: operation %s (%s) has no %s metadata tag; requiring a credential because %s is not a safe method",
		op.OperationID, op.Method, authRequiredKey, op.Method)
	return true
}

// appContextKey is where an authenticated app is stashed for handlers.
type appContextKey struct{}

// AppFromContext returns the app that authenticated this request, if any.
//
// Takes a context.Context rather than a huma.Context because the caller that
// matters is a handler, and huma hands handlers the plain context — a
// huma.Context signature would force every handler to convert. Middleware can
// still reach it via ctx.Context().
//
// Present on any request that carried a valid credential, including requests
// to public read operations — the middleware authenticates opportunistically
// so that a future per-app read budget has an identity to key on, and so the
// moderation queue can attribute a submission without the handler re-parsing
// anything. Absent means "no valid credential was presented", never "this
// operation does not need one".
func AppFromContext(ctx context.Context) (model.App, bool) {
	a, ok := ctx.Value(appContextKey{}).(model.App)
	return a, ok
}

// appAuthMiddleware resolves an app credential from the Authorization header,
// and enforces its presence on operations tagged with authRequiredKey.
//
// Two-stage on purpose. Authentication ("is this credential real?") runs for
// every request that presents one, so reads can be attributed later without a
// second code path. Authorization ("may this app write right now?") runs only
// where a write demands it, and is where suspension is enforced.
func appAuthMiddleware(hapi huma.API, s *store.Store) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		required := authRequiredFor(ctx.Operation())

		token, present := bearerToken(ctx.Header("Authorization"))
		if !present {
			if required {
				huma.WriteErr(hapi, ctx, http.StatusUnauthorized,
					"this endpoint requires an app credential: send `Authorization: Bearer <token>` — see docs/API_POLICY.md")
				return
			}
			next(ctx)
			return
		}

		app, err := authenticateApp(ctx, s, token)
		switch {
		case err == nil && app.Active():
			next(huma.WithValue(ctx, appContextKey{}, app))
			return

		case err != nil && !errors.Is(err, errBadCredential):
			// A real failure — the database is unreachable, not the
			// credential wrong. Never downgrade this to a 401: telling an app
			// its key is invalid during an outage sends it rotating a
			// perfectly good credential.
			log.Printf("app auth: %v", err)
			huma.WriteErr(hapi, ctx, http.StatusInternalServerError, "internal error")
			return
		}

		// From here the credential is either unusable or belongs to a
		// suspended app. On an operation that does not need one, neither is
		// this request's problem: the read API has been open to anonymous
		// traffic since Phase 1, and a client that attaches an unrelated
		// Authorization header — an API gateway injecting its own JWT, an app
		// forwarding an end-user session token by mistake — must not lose
		// access to reads it could make by simply dropping the header.
		// Suspension in particular is a write-side lever (see model.App and
		// migration 0003); enforcing it here would turn a moderation decision
		// into a total read blackout for that app's users.
		if !required {
			next(ctx)
			return
		}

		if err != nil {
			// Deliberately one message for a malformed token, an unknown key
			// id, and a wrong secret. Distinguishing them tells an attacker
			// which half of a guess was right, and tells a legitimate
			// operator nothing they can act on differently — in every case
			// the answer is "check the credential".
			huma.WriteErr(hapi, ctx, http.StatusUnauthorized, "invalid app credential")
			return
		}

		// A suspended app authenticated successfully — the credential is
		// real. Saying "invalid credential" here would send its operator
		// hunting for a key problem that does not exist, so this is a
		// distinct 403 with a reason they can act on.
		huma.WriteErr(hapi, ctx, http.StatusForbidden,
			"this app is suspended and cannot submit; contact the maintainer listed in docs/API_POLICY.md")
	}
}

// errBadCredential covers every way a presented credential fails to identify
// an active app, collapsed into one error so callers cannot accidentally
// report which way.
var errBadCredential = errors.New("bad app credential")

// authenticateApp resolves a presented token to its app row.
//
// A single store call fetches the row and its hash together: this runs on
// every authenticated request, and two round trips here would halve the rate
// the connection pool can sustain.
func authenticateApp(ctx huma.Context, s *store.Store, token string) (model.App, error) {
	keyID, secret, err := auth.ParseToken(token)
	if err != nil {
		return model.App{}, errBadCredential
	}

	app, hash, err := s.Apps.GetByKeyIDWithHash(ctx.Context(), keyID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Verify against a hash that cannot match, rather than returning
			// here. An early return would make "unknown key id" measurably
			// faster than "wrong secret", turning response time into an
			// oracle for which key ids exist.
			auth.VerifySecret(secret, unknownKeyHash)
			return model.App{}, errBadCredential
		}
		return model.App{}, err
	}

	if !auth.VerifySecret(secret, hash) {
		return model.App{}, errBadCredential
	}
	return app, nil
}

// unknownKeyHash is a syntactically valid hash no secret hashes to, used to
// spend the same work on an unknown key id as on a real one. Its value is
// irrelevant beyond being the right length and never matching.
const unknownKeyHash = "0000000000000000000000000000000000000000000000000000000000000000"

// bearerToken extracts the credential from an Authorization header, reporting
// whether one was present at all.
//
// Case-insensitive on the scheme per RFC 9110 §11.1, which specifies auth
// schemes as case-insensitive — a client sending `bearer` is following the
// spec, and rejecting it would be a confusing failure to debug.
func bearerToken(header string) (string, bool) {
	const scheme = "bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(header[len(scheme):])
	if token == "" {
		return "", false
	}
	return token, true
}
