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

// authRequiredKey is the huma.Operation.Metadata key a write operation sets
// to mark itself as requiring a credential, mirroring rateLimitTierKey's
// tagging scheme in ratelimit.go.
//
// Opt-in rather than opt-out, the reverse of the rate limiter's fail-closed
// default, because the two failure modes are not symmetric here. Every
// operation in this package today is a public read; defaulting to "auth
// required" would mean a forgotten tag takes the public API offline, while
// defaulting to "public" means a forgotten tag on a *write* leaves it open.
// The second is the more dangerous error — so writes do not rely on the
// default at all: registerWrite below sets the tag, and no write operation is
// registered any other way.
const authRequiredKey = "oedb-auth-required"

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
		required, _ := ctx.Operation().Metadata[authRequiredKey].(bool)

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
		if err != nil {
			if errors.Is(err, errBadCredential) {
				// Deliberately one message for a malformed token, an unknown
				// key id, and a wrong secret. Distinguishing them tells an
				// attacker which half of a guess was right, and tells a
				// legitimate operator nothing they can act on differently —
				// in every case the answer is "check the credential".
				huma.WriteErr(hapi, ctx, http.StatusUnauthorized, "invalid app credential")
				return
			}
			log.Printf("app auth: %v", err)
			huma.WriteErr(hapi, ctx, http.StatusInternalServerError, "internal error")
			return
		}

		// A suspended app authenticated successfully — the credential is
		// real. Saying "invalid credential" here would send its operator
		// hunting for a key problem that does not exist, so this is a
		// distinct 403 with a reason they can act on.
		if !app.Active() {
			huma.WriteErr(hapi, ctx, http.StatusForbidden,
				"this app is suspended and cannot submit; contact the maintainer listed in docs/API_POLICY.md")
			return
		}

		next(huma.WithValue(ctx, appContextKey{}, app))
	}
}

// errBadCredential covers every way a presented credential fails to identify
// an active app, collapsed into one error so callers cannot accidentally
// report which way.
var errBadCredential = errors.New("bad app credential")

// authenticateApp resolves a presented token to its app row.
func authenticateApp(ctx huma.Context, s *store.Store, token string) (model.App, error) {
	keyID, secret, err := auth.ParseToken(token)
	if err != nil {
		return model.App{}, errBadCredential
	}

	hash, err := s.Apps.GetKeyHashByKeyID(ctx.Context(), keyID)
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

	app, err := s.Apps.GetByKeyID(ctx.Context(), keyID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The row existed a moment ago and does not now — deleted
			// mid-request. Treat as a bad credential rather than a 500: the
			// credential genuinely is no longer valid.
			return model.App{}, errBadCredential
		}
		return model.App{}, err
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
