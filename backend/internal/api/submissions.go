package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/Cawlumm/open-exercise-db/internal/contract"
	"github.com/Cawlumm/open-exercise-db/internal/model"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// capWindow is the period the per-app daily cap is measured over. A rolling
// window rather than a calendar day: a calendar reset lets an app spend its
// whole budget at 23:59 and again at 00:01, which is the burst the cap exists
// to prevent, and it would do so on a boundary that depends on the server's
// timezone rather than on anything the app can reason about.
const capWindow = 24 * time.Hour

// maxPayloadBytes bounds a single submission's payload.
//
// The queue holds proposed content a human will read, and the largest
// legitimate submission is an exercise with instructions — kilobytes, not
// megabytes. Without a bound, one app can fill the moderation queue's storage
// with rows that are individually valid and collectively unreviewable, which
// the per-app count caps do not address at all: 50 pending submissions of
// 10MB each is inside every limit and is still a denial of service against
// the moderator.
const maxPayloadBytes = 64 * 1024

// CreateSubmissionInput is the body an app posts to propose a change.
type CreateSubmissionInput struct {
	Body struct {
		Type string `json:"type" enum:"create,edit,translation" doc:"What kind of change this proposes"`

		TargetExerciseID *string `json:"target_exercise_id,omitempty" format:"uuid" doc:"Required for edit and translation; omitted for create"`
		Locale           *string `json:"locale,omitempty" maxLength:"35" doc:"Required for translation; the BCP-47 locale being translated into"`

		Payload json.RawMessage `json:"payload" doc:"The proposed content, shaped like the exercise it will become"`

		// Optional, but strongly encouraged: without it the only lever
		// against one bad contributor inside an app is suspending the whole
		// app. Not required, because an app that genuinely has no per-user
		// concept should not be forced to invent one.
		AppUserRef *string `json:"app_user_ref,omitempty" minLength:"1" maxLength:"128" doc:"Your own opaque handle for the contributor; never stored as an identity here"`

		// Ids from POST /images. Validated against this app's own uploads —
		// see checkImageOwnership for why attaching someone else's is not
		// merely disallowed but actively dangerous.
		ImageUploadIDs []string `json:"image_upload_ids,omitempty" maxItems:"8" doc:"Ids returned by POST /api/v1/images"`
	}
}

type CreateSubmissionOutput struct {
	Status int
	Body   contract.Submission
}

// ListMySubmissionsInput pages an app's own queue.
type ListMySubmissionsInput struct {
	AppUserRef string `query:"app_user_ref" doc:"Narrow to one of your own contributors"`
	PageParams
}

type ListMySubmissionsOutput struct {
	Body struct {
		Submissions []contract.Submission `json:"submissions"`
		Pagination
	}
}

// registerSubmissionRoutes wires the write half of the API.
//
// Every operation here sets authRequiredKey explicitly. The middleware would
// require a credential anyway — POST is not a safe method, so authRequiredFor
// fails closed — but relying on that would leave the requirement undocumented
// at the registration site and absent from the generated OpenAPI security
// description. The tag states the intent; the method default is the backstop
// for when someone forgets.
func registerSubmissionRoutes(hapi huma.API, s *store.Store) {
	huma.Register(hapi, huma.Operation{
		OperationID: "create-submission",
		Method:      http.MethodPost,
		Path:        "/api/v1/submissions",
		Summary:     "Propose a new exercise, an edit, or a translation",
		Description: "Queues a change for human review. Nothing submitted here is visible through the read API until a moderator approves it.",
		Tags:        []string{"submissions"},
		Metadata: map[string]any{
			rateLimitTierKey: ratelimit.TierSearch,
			authRequiredKey:  true,
		},
		Errors: []int{
			http.StatusBadRequest,
			http.StatusUnauthorized,
			http.StatusForbidden,
			http.StatusUnprocessableEntity,
			http.StatusTooManyRequests,
		},
	}, func(ctx context.Context, in *CreateSubmissionInput) (*CreateSubmissionOutput, error) {
		app, ok := AppFromContext(ctx)
		if !ok {
			// Unreachable while the middleware is installed — it refuses this
			// operation without a credential. Checked anyway because the
			// alternative is attributing a submission to the zero value of an
			// App, which would insert an empty submitted_by_app and violate
			// the attribution CHECK as a 500 rather than being caught here.
			return nil, huma.Error401Unauthorized("this endpoint requires an app credential")
		}

		if err := validateSubmissionBody(in); err != nil {
			return nil, err
		}
		// Field-level validation against the live vocabulary. Structural
		// checks above only prove the payload is a JSON object; this is what
		// keeps a muscle group nobody can filter by out of the queue.
		// Translations are exempt: they carry a name and instructions in
		// another language, not taxonomy, and running them through the
		// English vocabulary would reject every one of them.
		if in.Body.Type != model.SubmissionTypeTranslation {
			if err := validatePayloadFields(ctx, s, in.Body.Type, in.Body.Payload); err != nil {
				return nil, err
			}
		}

		// A target that does not exist is the caller's mistake, not a server
		// fault. Left unchecked the insert violates the foreign key and
		// surfaces as a 500, telling an integrator their request broke the
		// server when they simply named a stale id.
		if in.Body.TargetExerciseID != nil {
			if _, err := s.Exercises.Get(ctx, *in.Body.TargetExerciseID); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, huma.Error422UnprocessableEntity("target_exercise_id names no exercise")
				}
				return nil, mapStoreError(err, "")
			}
		}

		if err := checkImageOwnership(ctx, s, app, in.Body.ImageUploadIDs); err != nil {
			return nil, err
		}

		usage, err := s.Apps.UsageFor(ctx, app.ID, derefOr(in.Body.AppUserRef, ""), capWindow)
		if err != nil {
			return nil, mapStoreError(err, "")
		}
		if err := checkCaps(app, usage); err != nil {
			return nil, err
		}

		sub, err := s.Submissions.CreateAppSubmission(ctx, store.CreateAppSubmissionInput{
			Type:             in.Body.Type,
			TargetExerciseID: in.Body.TargetExerciseID,
			Locale:           in.Body.Locale,
			Payload:          in.Body.Payload,
			AppID:            app.ID,
			AppUserRef:       in.Body.AppUserRef,
			ImageUploadIDs:   in.Body.ImageUploadIDs,
		})
		if err != nil {
			return nil, mapStoreError(err, "")
		}

		return &CreateSubmissionOutput{Status: http.StatusCreated, Body: toContractSubmission(sub)}, nil
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "list-my-submissions",
		Method:      http.MethodGet,
		Path:        "/api/v1/submissions/mine",
		Summary:     "List your own queued submissions",
		Description: "Scoped to the authenticated app. Never returns another app's submissions.",
		Tags:        []string{"submissions"},
		Metadata: map[string]any{
			rateLimitTierKey: ratelimit.TierRead,
			// A GET, so the method default would make this public. It is not:
			// an app's queue is its own, and this is exactly the case the
			// explicit tag exists for.
			authRequiredKey: true,
		},
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests},
	}, func(ctx context.Context, in *ListMySubmissionsInput) (*ListMySubmissionsOutput, error) {
		app, ok := AppFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("this endpoint requires an app credential")
		}

		var ref *string
		if in.AppUserRef != "" {
			ref = &in.AppUserRef
		}

		_, perPage := in.Normalize()
		subs, total, err := s.Submissions.ListByApp(ctx, app.ID, ref, perPage, in.Offset())
		if err != nil {
			return nil, mapStoreError(err, "")
		}

		out := &ListMySubmissionsOutput{}
		out.Body.Pagination = paginationFor(in.PageParams, total)
		// Empty slice, not nil — see ListExercises (exercises.go).
		out.Body.Submissions = make([]contract.Submission, 0, len(subs))
		for _, sub := range subs {
			out.Body.Submissions = append(out.Body.Submissions, toContractSubmission(sub))
		}
		return out, nil
	})
}

// validateSubmissionBody enforces the rules the database also enforces, so a
// bad request is a 422 naming the field rather than a 500 carrying a
// constraint name.
//
// Deliberately not a replacement for the CHECKs: the constraints are what
// actually guarantee the invariants, including against any future writer that
// does not come through this handler.
func validateSubmissionBody(in *CreateSubmissionInput) error {
	if !model.ValidSubmissionType(in.Body.Type) {
		return huma.Error422UnprocessableEntity(fmt.Sprintf("type must be one of create, edit, translation (got %q)", in.Body.Type))
	}

	if in.Body.Type != model.SubmissionTypeCreate && in.Body.TargetExerciseID == nil {
		return huma.Error422UnprocessableEntity("target_exercise_id is required for edit and translation submissions")
	}
	if in.Body.Type == model.SubmissionTypeCreate && in.Body.TargetExerciseID != nil {
		// Rejected rather than ignored: a caller sending both has a
		// misunderstanding worth surfacing, and silently dropping the field
		// would let them believe an edit was queued against that exercise.
		return huma.Error422UnprocessableEntity("target_exercise_id must be omitted for create submissions")
	}
	if in.Body.Type == model.SubmissionTypeTranslation && in.Body.Locale == nil {
		return huma.Error422UnprocessableEntity("locale is required for translation submissions")
	}

	if len(in.Body.Payload) == 0 {
		return huma.Error422UnprocessableEntity("payload is required")
	}
	if len(in.Body.Payload) > maxPayloadBytes {
		return huma.Error422UnprocessableEntity(fmt.Sprintf("payload is %d bytes; the limit is %d", len(in.Body.Payload), maxPayloadBytes))
	}

	// A JSON object specifically, not any valid JSON. `payload: "hello"` and
	// `payload: [1,2]` are both well-formed and neither can ever become an
	// exercise, so accepting them only defers the failure to a moderator
	// looking at an unreviewable row.
	var probe map[string]any
	if err := json.Unmarshal(in.Body.Payload, &probe); err != nil {
		return huma.Error422UnprocessableEntity("payload must be a JSON object")
	}
	if len(probe) == 0 {
		return huma.Error422UnprocessableEntity("payload must not be empty")
	}
	return nil
}

// checkCaps refuses a submission that would exceed the app's budget.
//
// 429 rather than 403: this is a rate limit, it is temporary, and the caller
// should retry later — which is what 429 means and what its Retry-After
// convention tells clients to do. A 403 would read as "never allowed" and
// send an integrator to the maintainer over what resolves itself.
func checkCaps(app model.App, usage store.AppUsage) error {
	if usage.Pending >= app.MaxPending {
		return huma.Error429TooManyRequests(fmt.Sprintf(
			"you have %d submissions awaiting review and the limit is %d; wait for some to be reviewed before sending more",
			usage.Pending, app.MaxPending))
	}
	if usage.LastDay >= app.MaxPerDay {
		return huma.Error429TooManyRequests(fmt.Sprintf(
			"you have sent %d submissions in the last 24 hours and the limit is %d",
			usage.LastDay, app.MaxPerDay))
	}
	return nil
}

// toContractSubmission narrows a stored submission to its public shape,
// dropping reviewer identity and notes.
func toContractSubmission(s model.Submission) contract.Submission {
	return contract.Submission{
		ID:               s.ID,
		Type:             s.Type,
		TargetExerciseID: s.TargetExerciseID,
		Locale:           s.Locale,
		Payload:          s.Payload,
		AppUserRef:       s.AppUserRef,
		Status:           s.Status,
		CreatedAt:        s.CreatedAt,
		UpdatedAt:        s.UpdatedAt,
	}
}

// checkImageOwnership refuses image ids this app did not upload.
//
// Not merely an authorization nicety. Approving a submission publishes every
// image it references, so attaching another app's pending upload would let one
// app get a stranger's unreviewed content published under a submission a
// moderator was looking at for other reasons — the image is the part nobody
// re-reads once the text looks fine.
//
// Ownership is the storage-key prefix POST /api/v1/images writes, which is the
// only link between an upload and its uploader today: exercise_images.
// uploaded_by references users, and an app is not a user.
func checkImageOwnership(ctx context.Context, s *store.Store, app model.App, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	owned, err := s.Media.CountImagesOwnedByApp(ctx, app.ID, ids)
	if err != nil {
		return mapStoreError(err, "")
	}
	if owned != len(ids) {
		// One message for "does not exist" and "belongs to someone else": the
		// difference would tell a caller which ids are real, which is an
		// enumeration oracle over other apps' pending uploads.
		return huma.Error422UnprocessableEntity(
			"image_upload_ids must all be images you uploaded and have not yet had published")
	}
	return nil
}

func derefOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}
