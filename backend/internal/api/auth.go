package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

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

// roleRequiredKey is the huma.Operation.Metadata key an operation sets to
// demand a minimum role. Only meaningful for user credentials: an app has no
// role and can never satisfy one, which is deliberate — moderation is a
// judgement a person makes, and an app holding a credential is not a person.
const roleRequiredKey = "oedb-role-required"

// AuthConfig is the authentication settings Register needs.
//
// A struct rather than more positional parameters: Register already takes six,
// and sign-in adds a secret plus an OAuth client that several handlers need.
// Grouping them means adding the next auth setting does not touch every call
// site again.
type AuthConfig struct {
	// JWTSecret signs session tokens and OAuth state. Empty disables user
	// authentication entirely — app credentials still work, which is what
	// keeps most tests from needing to care.
	JWTSecret string

	// GitHubOAuth is the sign-in client. Its zero value is "not configured",
	// and the sign-in routes report that as unavailable rather than failing
	// as though the user did something wrong.
	GitHubOAuth auth.GitHubOAuth

	// AdminGitHubIDs is the raw ADMIN_GITHUB_IDS value: numeric GitHub user ids.
	// When non-empty it is the authoritative admin list and AdminGitHubLogins is
	// ignored. Parsed at sign-in, like the logins, so a change needs no redeploy
	// of code.
	AdminGitHubIDs string

	// AdminGitHubLogins is the deprecated ADMIN_GITHUB_LOGINS value. Parsed at
	// sign-in rather than at startup so changing it takes effect on the next
	// login without a redeploy — which matters when the reason you are
	// changing it is that you locked yourself out.
	AdminGitHubLogins string

	// TrustedProxyMode selects how a request's client address is derived (see
	// clientKey). Empty means direct: no forwarding header is believed.
	TrustedProxyMode string

	// unknownKeys remembers recently-seen key ids that are not in the database;
	// set by appAuthMiddleware.
	unknownKeys *negativeKeyCache
}

// appContextKey is where an authenticated app is stashed for handlers.
type appContextKey struct{}

// userContextKey is where an authenticated person is stashed for handlers.
type userContextKey struct{}

// UserFromContext returns the person that authenticated this request, if any.
//
// Mutually exclusive with AppFromContext: a request carries one credential, so
// at most one of these returns a value. A handler that needs "whoever this is"
// should check both rather than assuming, and a handler that only makes sense
// for one should say so by checking only that one.
func UserFromContext(ctx context.Context) (model.User, bool) {
	u, ok := ctx.Value(userContextKey{}).(model.User)
	return u, ok
}

// roleRequiredFor returns the minimum role an operation demands, or "".
func roleRequiredFor(op *huma.Operation) string {
	role, _ := op.Metadata[roleRequiredKey].(string)
	return role
}

// satisfiesRole reports whether u meets a required role.
func satisfiesRole(u model.User, required string) bool {
	switch required {
	case "":
		return true
	case model.RoleModerator:
		return u.CanModerate()
	case model.RoleAdmin:
		return u.IsAdmin()
	default:
		// An unknown role requirement denies rather than allows. A typo in a
		// metadata tag must not silently open a moderation endpoint.
		return false
	}
}

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
func appAuthMiddleware(hapi huma.API, s *store.Store, cfg AuthConfig) func(huma.Context, func(huma.Context)) {
	cfg.unknownKeys = newNegativeKeyCache(negativeKeyTTL, negativeKeyMax)
	return func(ctx huma.Context, next func(huma.Context)) {
		op := ctx.Operation()
		required := authRequiredFor(op)
		requiredRole := roleRequiredFor(op)

		token, present := bearerToken(ctx.Header("Authorization"))
		if !present {
			if required {
				huma.WriteErr(hapi, ctx, http.StatusUnauthorized,
					"this endpoint requires a credential: send `Authorization: Bearer <token>` — see docs/API_POLICY.md")
				return
			}
			next(ctx)
			return
		}

		app, user, err := authenticate(ctx, s, cfg, token)
		switch {
		case err != nil && !errors.Is(err, errBadCredential):
			// A real failure — the database is unreachable, not the
			// credential wrong. Never downgrade this to a 401: telling a
			// caller its key is invalid during an outage sends it rotating a
			// perfectly good credential.
			log.Printf("auth: %v", err)
			huma.WriteErr(hapi, ctx, http.StatusInternalServerError, "internal error")
			return

		case err == nil && user != nil:
			if !satisfiesRole(*user, requiredRole) {
				if !required {
					next(ctx)
					return
				}
				// The credential is real and the person is who they say they
				// are; they simply are not a moderator. Distinct from 401 for
				// the same reason suspension is: rotating a token changes
				// nothing here.
				huma.WriteErr(hapi, ctx, http.StatusForbidden,
					"this endpoint requires the "+requiredRole+" role")
				return
			}
			next(huma.WithValue(ctx, userContextKey{}, *user))
			return

		case err == nil && app != nil && app.Active():
			// An app can never satisfy a role requirement — it is not a
			// person, and moderation is a judgement a person makes.
			if requiredRole != "" {
				if !required {
					next(ctx)
					return
				}
				huma.WriteErr(hapi, ctx, http.StatusForbidden,
					"this endpoint requires the "+requiredRole+" role, which an app credential cannot hold")
				return
			}
			next(huma.WithValue(ctx, appContextKey{}, *app))
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
			// Deliberately one message for every failure — a malformed token,
			// an unknown key id, a wrong secret, an expired session, a forged
			// signature. Distinguishing them tells an attacker which half of
			// a guess was right, and would also reveal whether a given token
			// is an app credential or a user session. In every case the
			// answer is "check the credential".
			huma.WriteErr(hapi, ctx, http.StatusUnauthorized, "invalid credential")
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

// authenticate resolves a presented token to whichever kind of principal it
// names. Exactly one of the returned pointers is non-nil on success.
//
// The token's own prefix selects the path, which leaks nothing — the client
// sent it. What matters is that both paths fail identically, so a caller
// cannot learn from a rejection whether the value it guessed was even the
// right *kind* of credential.
func authenticate(ctx huma.Context, s *store.Store, cfg AuthConfig, token string) (*model.App, *model.User, error) {
	if strings.HasPrefix(token, auth.AppTokenPrefix) {
		app, err := authenticateApp(ctx, s, cfg.unknownKeys, token)
		if err != nil {
			return nil, nil, err
		}
		return &app, nil, nil
	}

	user, err := authenticateUser(ctx, s, cfg, token)
	if err != nil {
		return nil, nil, err
	}
	return nil, &user, nil
}

// authenticateUser verifies a session token and loads the person it names.
//
// The user is loaded on every request rather than trusted from the token,
// because the token carries no role (see auth.Claims). That is one indexed
// lookup in exchange for a demotion or a deleted account taking effect
// immediately instead of whenever the token happened to expire.
func authenticateUser(ctx huma.Context, s *store.Store, cfg AuthConfig, token string) (model.User, error) {
	claims, err := auth.VerifyUserToken(cfg.JWTSecret, token, time.Now())
	if err != nil {
		// An unset secret means sign-in is not configured on this deployment.
		// It is still a bad credential from the caller's perspective — there
		// is no session to have — and reporting it distinctly would advertise
		// the misconfiguration to anyone who asked.
		return model.User{}, errBadCredential
	}

	user, err := s.Users.GetByID(ctx.Context(), claims.Subject)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// A validly-signed token for an account that no longer exists.
			// Deleting a user must revoke their sessions immediately, which
			// is exactly what this does.
			return model.User{}, errBadCredential
		}
		return model.User{}, err
	}
	return user, nil
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
func authenticateApp(ctx huma.Context, s *store.Store, unknown *negativeKeyCache, token string) (model.App, error) {
	keyID, secret, err := auth.ParseToken(token)
	if err != nil {
		return model.App{}, errBadCredential
	}

	// A key id that was just looked up and not found is answered without
	// another query. Real key ids are random, so one missing a moment ago does
	// not appear a moment later; this stops a loop of forged ids that stays
	// under the flood guard from costing a query each.
	if unknown.seen(keyID, time.Now()) {
		auth.VerifySecret(secret, unknownKeyHash)
		return model.App{}, errBadCredential
	}

	app, hash, err := s.Apps.GetByKeyIDWithHash(ctx.Context(), keyID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			unknown.add(keyID, time.Now())
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
