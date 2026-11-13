package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/Cawlumm/open-exercise-db/internal/auth"
	"github.com/Cawlumm/open-exercise-db/internal/config"
	"github.com/Cawlumm/open-exercise-db/internal/model"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// signInTier is what /auth/github and its callback are budgeted at.
//
// Not the export tier, which was the first choice and was wrong: a single
// sign-in attempt costs two requests against it, so burst 2 meant anyone who
// abandoned a login and started again was throttled on their second attempt —
// found by walking the flow by hand rather than by any test.
//
// The search tier instead. The callback does cost a round trip to GitHub, but
// it cannot be reached without a state this API signed and issued within the
// last ten minutes, so the expensive half is already gated behind the cheap
// half.
const signInTier = ratelimit.TierSearch

// Me is the public shape of the signed-in person.
//
// GitHubID is deliberately absent: it identifies the account on another
// service and nothing here needs it, so publishing it would be a gratuitous
// correlation handle.
type Me struct {
	ID        string  `json:"id"`
	Username  string  `json:"username"`
	AvatarURL *string `json:"avatar_url,omitempty"`
	Role      string  `json:"role"`
}

type StartSignInOutput struct {
	Status   int
	Location string `header:"Location"`
}

type SignInCallbackInput struct {
	Code  string `query:"code" doc:"Authorization code returned by GitHub"`
	State string `query:"state" doc:"Opaque value this API issued when the sign-in began"`
	Error string `query:"error" doc:"Set by GitHub when the user declined"`
}

type SignInCallbackOutput struct {
	Body struct {
		Token     string    `json:"token" doc:"Session token; send as Authorization: Bearer"`
		ExpiresAt time.Time `json:"expires_at"`
		User      Me        `json:"user"`
	}
}

type MeOutput struct {
	Body Me
}

// registerAuthRoutes wires GitHub sign-in and the identity echo.
func registerAuthRoutes(hapi huma.API, s *store.Store, cfg AuthConfig) {
	huma.Register(hapi, huma.Operation{
		OperationID: "start-github-signin",
		Method:      http.MethodGet,
		Path:        "/api/v1/auth/github",
		Summary:     "Begin GitHub sign-in",
		Description: "Redirects to GitHub. Only people sign in; apps authenticate with their own credentials instead.",
		Tags:        []string{"auth"},
		Metadata: map[string]any{
			rateLimitTierKey: signInTier,
			authRequiredKey:  false,
		},
		Errors: []int{http.StatusServiceUnavailable, http.StatusTooManyRequests},
	}, func(ctx context.Context, _ *struct{}) (*StartSignInOutput, error) {
		url, err := cfg.GitHubOAuth.AuthorizeURL(cfg.JWTSecret, time.Now())
		if err != nil {
			if errors.Is(err, auth.ErrOAuthNotConfigured) || errors.Is(err, auth.ErrNoSecret) {
				// A deployment that did not configure sign-in, not a caller
				// mistake. 503 rather than 500 so it reads as "this feature
				// is off here", which is what a self-hoster running without
				// OAuth credentials actually wants it to say.
				return nil, huma.Error503ServiceUnavailable("sign-in is not configured on this server")
			}
			log.Printf("auth: build authorize url: %v", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		return &StartSignInOutput{Status: http.StatusFound, Location: url}, nil
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "github-signin-callback",
		Method:      http.MethodGet,
		Path:        "/api/v1/auth/github/callback",
		Summary:     "Complete GitHub sign-in",
		Description: "Exchanges the authorization code for a session token.",
		Tags:        []string{"auth"},
		Metadata: map[string]any{
			rateLimitTierKey: signInTier,
			authRequiredKey:  false,
		},
		Errors: []int{
			http.StatusBadRequest,
			http.StatusUnauthorized,
			http.StatusServiceUnavailable,
			http.StatusTooManyRequests,
		},
	}, func(ctx context.Context, in *SignInCallbackInput) (*SignInCallbackOutput, error) {
		if !cfg.GitHubOAuth.Configured() || cfg.JWTSecret == "" {
			return nil, huma.Error503ServiceUnavailable("sign-in is not configured on this server")
		}
		if in.Error != "" {
			// The user declined on GitHub's consent screen. Not an error
			// condition of ours, and not worth a 500.
			return nil, huma.Error400BadRequest("sign-in was cancelled")
		}
		if in.Code == "" {
			return nil, huma.Error400BadRequest("missing authorization code")
		}

		// State first, before spending a network call on the code. A callback
		// that fails CSRF verification should cost nothing.
		if err := auth.VerifyState(cfg.JWTSecret, in.State, time.Now()); err != nil {
			return nil, huma.Error400BadRequest("this sign-in link is invalid or has expired; start again")
		}

		ghUser, err := cfg.GitHubOAuth.Exchange(ctx, in.Code)
		if err != nil {
			if errors.Is(err, auth.ErrExchangeFailed) {
				return nil, huma.Error401Unauthorized("GitHub rejected this sign-in; start again")
			}
			log.Printf("auth: github exchange: %v", err)
			return nil, huma.Error500InternalServerError("internal error")
		}

		var avatar *string
		if ghUser.AvatarURL != "" {
			avatar = &ghUser.AvatarURL
		}

		// Promotion is decided here from configuration and passed to the
		// store as a boolean, so nothing GitHub returns can influence a role.
		ghID := githubIDString(ghUser.ID)
		var promote, demote bool
		if ids := model.AdminGitHubIDs(cfg.AdminGitHubIDs); len(ids) > 0 {
			// Authoritative: the id comes from GitHub's own user response, is
			// immutable, and a user not on the list loses a stale admin role.
			promote = ids[ghID]
			demote = !promote
		} else {
			// Deprecated fallback; usernames can be renamed and re-registered,
			// so it stops working at config.LoginPromotionSunset.
			promote = config.LoginPromotionAllowed(time.Now()) &&
				model.AdminLogins(cfg.AdminGitHubLogins)[lower(ghUser.Login)]
		}
		user, err := s.Users.UpsertGitHubUser(ctx, store.UpsertGitHubUserInput{
			GitHubID:       ghID,
			Username:       ghUser.Login,
			AvatarURL:      avatar,
			PromoteToAdmin: promote,
			DemoteAdmin:    demote,
		})
		if err != nil {
			return nil, mapStoreError(err, "")
		}

		token, err := auth.MintUserToken(cfg.JWTSecret, user.ID, time.Now())
		if err != nil {
			log.Printf("auth: mint token: %v", err)
			return nil, huma.Error500InternalServerError("internal error")
		}

		out := &SignInCallbackOutput{}
		out.Body.Token = token
		out.Body.ExpiresAt = time.Now().Add(auth.TokenTTL())
		out.Body.User = toMe(user)
		return out, nil
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "get-me",
		Method:      http.MethodGet,
		Path:        "/api/v1/me",
		Summary:     "Who am I",
		Description: "Echoes the signed-in person, including their current role.",
		Tags:        []string{"auth"},
		Metadata: map[string]any{
			// One indexed lookup, so the read tier. It shared the export
			// tier briefly, which put a trivial identity echo on the same
			// 5/min budget as the 1.8MB full-dataset dump.
			rateLimitTierKey: ratelimit.TierRead,
			// A GET the method default would leave public, so it says so
			// explicitly: there is no sensible anonymous answer.
			authRequiredKey: true,
		},
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests},
	}, func(ctx context.Context, _ *struct{}) (*MeOutput, error) {
		user, ok := UserFromContext(ctx)
		if !ok {
			// Reachable with a valid *app* credential, which authenticates
			// but is not a person. A distinct message, because "invalid
			// credential" would be wrong and confusing — theirs works fine.
			return nil, huma.Error403Forbidden("this endpoint describes a signed-in person; app credentials have no profile")
		}
		return &MeOutput{Body: toMe(user)}, nil
	})
}

func toMe(u model.User) Me {
	return Me{ID: u.ID, Username: u.Username, AvatarURL: u.AvatarURL, Role: u.Role}
}

// githubIDString renders GitHub's numeric account id as the text the
// users.github_id column stores. Text rather than an integer column because a
// future second provider would otherwise need a second column, and because an
// id is an identifier rather than a number anyone does arithmetic on.
func githubIDString(id int64) string { return strconv.FormatInt(id, 10) }

func lower(s string) string { return strings.ToLower(s) }
