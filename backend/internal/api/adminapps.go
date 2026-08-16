package api

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Cawlumm/open-exercise-db/internal/auth"
	"github.com/Cawlumm/open-exercise-db/internal/model"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// AdminApp is the public shape of a registered app.
//
// No key_hash, and no token. The hash is secret material and the token exists
// exactly once, in the response to the request that created or rotated it —
// see RegisteredApp.
type AdminApp struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ContactEmail string `json:"contact_email"`
	KeyID        string `json:"key_id" doc:"Public half of the credential; identifies the app in logs"`
	Status       string `json:"status"`
	MaxPending   int    `json:"max_pending"`
	MaxPerDay    int    `json:"max_per_day"`
}

// RegisteredApp carries the one and only copy of a new credential.
type RegisteredApp struct {
	AdminApp
	Token string `json:"token" doc:"Shown once and never recoverable. Store it now."`
}

type RegisterAppInput struct {
	Body struct {
		Name         string `json:"name" minLength:"1" maxLength:"64" doc:"Display name, shown beside this app's submissions in the queue"`
		ContactEmail string `json:"contact_email" minLength:"3" maxLength:"254" doc:"Who to reach when this app misbehaves"`

		// Optional. Omitted means the schema's defaults, which is the honest
		// way to say "whatever the migration says" — see store.CreateApp.
		MaxPending *int `json:"max_pending,omitempty" minimum:"1" maximum:"10000"`
		MaxPerDay  *int `json:"max_per_day,omitempty" minimum:"1" maximum:"100000"`
	}
}

type RegisterAppOutput struct {
	Status int
	Body   RegisteredApp
}

type RotateAppKeyInput struct {
	ID string `path:"id" format:"uuid"`
}

type SetAppStatusInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Status string `json:"status" enum:"active,suspended"`
	}
}

type AdminAppOutput struct {
	Body AdminApp
}

// adminMetadata is the tag set every admin operation shares.
//
// Both keys matter and neither implies the other: authRequiredKey makes the
// middleware demand a credential, roleRequiredKey makes it demand this
// particular role. An operation with only the second would still be reachable
// anonymously.
func adminMetadata(tier ratelimit.Tier) map[string]any {
	return map[string]any{
		rateLimitTierKey: tier,
		authRequiredKey:  true,
		roleRequiredKey:  model.RoleAdmin,
	}
}

// registerAdminAppRoutes wires app registration and lifecycle.
//
// Admin-only, and deliberately not self-service. Anyone able to mint an app
// can mint submission capacity, and the per-app caps assume an app is a
// somewhat-vetted entity: with open registration an abuser registers a hundred
// apps and has a hundred times the budget, which makes the caps meaningless
// against exactly the person they exist for. Open Food Facts can allow open
// signup because it publishes write-through and moderates afterwards; here the
// scarce resource is one maintainer's reading time.
func registerAdminAppRoutes(hapi huma.API, s *store.Store) {
	huma.Register(hapi, huma.Operation{
		OperationID: "register-app",
		Method:      http.MethodPost,
		Path:        "/api/v1/admin/apps",
		Summary:     "Register an app and issue its credential",
		Description: "Returns the app's token exactly once. It is not stored and cannot be recovered — only rotated.",
		Tags:        []string{"admin"},
		Metadata:    adminMetadata(ratelimit.TierSearch),
		Errors: []int{
			http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
			http.StatusConflict, http.StatusUnprocessableEntity, http.StatusTooManyRequests,
		},
	}, func(ctx context.Context, in *RegisterAppInput) (*RegisterAppOutput, error) {
		keyID, token, hash, err := auth.GenerateAppCredential()
		if err != nil {
			log.Printf("admin: generate app credential: %v", err)
			return nil, huma.Error500InternalServerError("internal error")
		}

		app, err := s.Apps.CreateApp(ctx, store.CreateAppInput{
			Name:         in.Body.Name,
			ContactEmail: in.Body.ContactEmail,
			KeyID:        keyID,
			KeyHash:      hash,
			MaxPending:   in.Body.MaxPending,
			MaxPerDay:    in.Body.MaxPerDay,
		})
		if err != nil {
			// A duplicate name is a conflict the caller can resolve, not an
			// internal error. Left as a 500 it would read as "the server is
			// broken" for what is really "pick another name".
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, huma.Error409Conflict("an app with that name already exists")
			}
			return nil, mapStoreError(err, "")
		}

		out := &RegisterAppOutput{Status: http.StatusCreated}
		out.Body.AdminApp = toAdminApp(app)
		out.Body.Token = token
		return out, nil
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "rotate-app-key",
		Method:      http.MethodPost,
		Path:        "/api/v1/admin/apps/{id}/rotate",
		Summary:     "Issue a new credential for an app",
		Description: "The previous credential stops working immediately. Attribution of past submissions is unaffected.",
		Tags:        []string{"admin"},
		Metadata:    adminMetadata(ratelimit.TierSearch),
		Errors: []int{
			http.StatusUnauthorized, http.StatusForbidden,
			http.StatusNotFound, http.StatusTooManyRequests,
		},
	}, func(ctx context.Context, in *RotateAppKeyInput) (*RegisterAppOutput, error) {
		keyID, token, hash, err := auth.GenerateAppCredential()
		if err != nil {
			log.Printf("admin: generate app credential: %v", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		if err := s.Apps.RotateKey(ctx, in.ID, keyID, hash); err != nil {
			return nil, mapAdminAppError(err)
		}

		app, err := s.Apps.GetByKeyID(ctx, keyID)
		if err != nil {
			return nil, mapStoreError(err, "no such app")
		}

		out := &RegisterAppOutput{Status: http.StatusOK}
		out.Body.AdminApp = toAdminApp(app)
		out.Body.Token = token
		return out, nil
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "set-app-status",
		Method:      http.MethodPost,
		Path:        "/api/v1/admin/apps/{id}/status",
		Summary:     "Suspend or reactivate an app",
		Description: "A suspended app cannot submit. Its reads are unaffected, and its queued submissions keep their attribution.",
		Tags:        []string{"admin"},
		Metadata:    adminMetadata(ratelimit.TierSearch),
		Errors: []int{
			http.StatusUnauthorized, http.StatusForbidden,
			http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusTooManyRequests,
		},
	}, func(ctx context.Context, in *SetAppStatusInput) (*AdminAppOutput, error) {
		if in.Body.Status != model.AppStatusActive && in.Body.Status != model.AppStatusSuspended {
			return nil, huma.Error422UnprocessableEntity("status must be active or suspended")
		}
		if err := s.Apps.SetStatus(ctx, in.ID, in.Body.Status); err != nil {
			return nil, mapAdminAppError(err)
		}
		app, err := s.Apps.GetByID(ctx, in.ID)
		if err != nil {
			return nil, mapStoreError(err, "no such app")
		}
		return &AdminAppOutput{Body: toAdminApp(app)}, nil
	})
}

// mapAdminAppError turns a missing app into a 404.
//
// AppStore's update methods report a missing row as store.ErrNoSuchApp rather
// than pgx.ErrNoRows, because an UPDATE affecting no rows is not a failed
// scan. mapStoreError only knows about ErrNoRows, so without this a rotate
// against a deleted app id would be a 500.
func mapAdminAppError(err error) error {
	if errors.Is(err, store.ErrNoSuchApp) || errors.Is(err, pgx.ErrNoRows) {
		return huma.Error404NotFound("no such app")
	}
	return mapStoreError(err, "no such app")
}

func toAdminApp(a model.App) AdminApp {
	return AdminApp{
		ID:           a.ID,
		Name:         a.Name,
		ContactEmail: a.ContactEmail,
		KeyID:        a.KeyID,
		Status:       a.Status,
		MaxPending:   a.MaxPending,
		MaxPerDay:    a.MaxPerDay,
	}
}
