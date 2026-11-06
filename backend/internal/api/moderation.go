package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/Cawlumm/open-exercise-db/internal/contract"
	"github.com/Cawlumm/open-exercise-db/internal/model"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// maxBulkDecisions bounds one bulk request.
//
// Bulk exists to make a queue tractable, not to let one request hold a
// connection for a minute: each decision is its own transaction, so 100 of
// them is 100 transactions, and an unbounded list would let a single call
// monopolise the pool. 100 is far more than a human reviews in one sitting
// and small enough to stay fast.
const maxBulkDecisions = 100

// moderatorMetadata is the tag set every moderation operation shares.
func moderatorMetadata(tier ratelimit.Tier) map[string]any {
	return map[string]any{
		rateLimitTierKey: tier,
		authRequiredKey:  true,
		roleRequiredKey:  model.RoleModerator,
	}
}

// QueuedSubmission is a submission as a moderator sees it.
//
// Wider than contract.Submission, which is what a *submitter* sees: this one
// carries the reviewer fields and the submitting app, because deciding
// requires knowing where something came from and whether it has been touched.
type QueuedSubmission struct {
	contract.Submission
	SubmittedByApp *string `json:"submitted_by_app,omitempty" doc:"Which app sent this, if any"`
	SubmittedBy    *string `json:"submitted_by,omitempty" doc:"Which person sent this, if any"`
	ReviewerID     *string `json:"reviewer_id,omitempty"`
	ReviewerNotes  *string `json:"reviewer_notes,omitempty"`
}

type ListQueueInput struct {
	Status string `query:"status" enum:"pending,approved,rejected,needs_changes" doc:"Defaults to pending"`
	Type   string `query:"type" enum:"create,edit,translation"`
	AppID  string `query:"app_id" doc:"Narrow to one submitting app"`
	PageParams
}

type ListQueueOutput struct {
	Body struct {
		Submissions []QueuedSubmission `json:"submissions"`
		Pagination
	}
}

type GetQueuedInput struct {
	ID string `path:"id" format:"uuid"`
}

// FieldDiff is one field that would change if this submission were approved.
type FieldDiff struct {
	Field    string `json:"field"`
	Current  any    `json:"current"`
	Proposed any    `json:"proposed"`
}

type GetQueuedOutput struct {
	Body struct {
		Submission QueuedSubmission `json:"submission"`

		// Diff is the point of this endpoint. Reviewing an edit by reading
		// the whole proposed payload against the whole current record is how
		// a moderator misses a one-word change buried in three paragraphs of
		// identical instructions.
		Diff []FieldDiff `json:"diff"`

		// Empty for a create, where nothing exists to compare against.
		HasCurrent bool `json:"has_current"`
	}
}

type DecisionInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Notes *string `json:"notes,omitempty" maxLength:"2000" doc:"Why, for the record and for the submitter"`
	}
}

type ApproveOutput struct {
	Body struct {
		SubmissionID string `json:"submission_id"`
		ExerciseID   string `json:"exercise_id"`
		RevisionID   string `json:"revision_id"`
		Created      bool   `json:"created" doc:"True when this published a brand new exercise"`
	}
}

type DecisionOutput struct {
	Body struct {
		SubmissionID string `json:"submission_id"`
		Status       string `json:"status"`
	}
}

type BulkDecisionInput struct {
	Body struct {
		IDs    []string `json:"ids" minItems:"1" maxItems:"100" doc:"Submissions to decide, in one call"`
		Action string   `json:"action" enum:"approve,reject"`
		Notes  *string  `json:"notes,omitempty" maxLength:"2000"`
	}
}

// BulkOutcome is one submission's result inside a bulk request.
type BulkOutcome struct {
	SubmissionID string `json:"submission_id"`
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty" doc:"Why this one failed; the others still happened"`
}

type BulkDecisionOutput struct {
	Body struct {
		Succeeded int           `json:"succeeded"`
		Failed    int           `json:"failed"`
		Outcomes  []BulkOutcome `json:"outcomes"`
	}
}

type RevertInput struct {
	ID         string `path:"id" format:"uuid" doc:"Exercise to revert"`
	RevisionID string `path:"revision_id" format:"uuid" doc:"Revision to make current again"`
}

type RevertOutput struct {
	Body struct {
		ExerciseID string `json:"exercise_id"`
		RevisionID string `json:"revision_id"`
	}
}

// registerModerationRoutes wires the moderation queue. invalidatePublished
// clears every in-process cache of published data (the export blob and the
// list-exercises responses); it is called after each write that changes what
// the public read endpoints return.
func registerModerationRoutes(hapi huma.API, s *store.Store, invalidatePublished func()) {
	huma.Register(hapi, huma.Operation{
		OperationID: "list-submission-queue",
		Method:      http.MethodGet,
		Path:        "/api/v1/admin/submissions",
		Summary:     "The moderation queue",
		Description: "Oldest first, because the submission most at risk of being forgotten is the one that has waited longest.",
		Tags:        []string{"moderation"},
		Metadata:    moderatorMetadata(ratelimit.TierRead),
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests},
	}, func(ctx context.Context, in *ListQueueInput) (*ListQueueOutput, error) {
		status := in.Status
		if status == "" {
			// Defaulting to pending rather than everything: the queue is a
			// worklist, and a default that includes decided items makes the
			// first page mostly noise on any instance with history.
			status = model.SubmissionStatusPending
		}
		_, perPage := in.Normalize()

		subs, total, err := s.Moderation.ListQueue(ctx, store.SubmissionFilter{
			Status: status, Type: in.Type, AppID: in.AppID,
			Limit: perPage, Offset: in.Offset(),
		})
		if err != nil {
			return nil, mapStoreError(err, "")
		}

		out := &ListQueueOutput{}
		out.Body.Pagination = paginationFor(in.PageParams, total)
		out.Body.Submissions = make([]QueuedSubmission, 0, len(subs))
		for _, sub := range subs {
			out.Body.Submissions = append(out.Body.Submissions, toQueued(sub))
		}
		return out, nil
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "get-queued-submission",
		Method:      http.MethodGet,
		Path:        "/api/v1/admin/submissions/{id}",
		Summary:     "One submission, with a diff against what is published now",
		Tags:        []string{"moderation"},
		Metadata:    moderatorMetadata(ratelimit.TierRead),
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests},
	}, func(ctx context.Context, in *GetQueuedInput) (*GetQueuedOutput, error) {
		sub, err := s.Moderation.GetSubmission(ctx, in.ID)
		if err != nil {
			return nil, mapStoreError(err, "no such submission")
		}

		out := &GetQueuedOutput{}
		out.Body.Submission = toQueued(sub)
		out.Body.Diff = []FieldDiff{}

		if sub.TargetExerciseID != nil {
			current, err := s.Exercises.Get(ctx, *sub.TargetExerciseID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, mapStoreError(err, "")
			}
			if err == nil {
				out.Body.HasCurrent = true
				out.Body.Diff = diffAgainstCurrent(current, sub.Payload)
			}
		}
		return out, nil
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "approve-submission",
		Method:      http.MethodPost,
		Path:        "/api/v1/admin/submissions/{id}/approve",
		Summary:     "Publish a submission",
		Description: "Materializes the proposal into the published tables. This is the only path from the queue into public data.",
		Tags:        []string{"moderation"},
		Metadata:    moderatorMetadata(ratelimit.TierSearch),
		Errors: []int{
			http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
			http.StatusConflict, http.StatusUnprocessableEntity, http.StatusTooManyRequests,
		},
	}, func(ctx context.Context, in *DecisionInput) (*ApproveOutput, error) {
		user, ok := UserFromContext(ctx)
		if !ok {
			return nil, huma.Error403Forbidden("moderation requires a signed-in person")
		}

		res, err := s.Moderation.Approve(ctx, store.Decision{
			SubmissionID: in.ID, ReviewerID: user.ID, Notes: in.Body.Notes,
		})
		if err != nil {
			return nil, mapModerationError(err)
		}

		// Approval is the only thing that changes published data, so it is
		// the only thing that has to tell the caches. Without this
		// /exercises/export and /exercises keep serving the pre-approval
		// dataset for up to export.DefaultCacheTTL — and mirrors, which consume
		// the dump precisely so they need not poll, take that stale copy as
		// authoritative and have no way to know a newer one exists.
		invalidatePublished()

		out := &ApproveOutput{}
		out.Body.SubmissionID = in.ID
		out.Body.ExerciseID = res.ExerciseID
		out.Body.RevisionID = res.RevisionID
		out.Body.Created = res.Created
		return out, nil
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "reject-submission",
		Method:      http.MethodPost,
		Path:        "/api/v1/admin/submissions/{id}/reject",
		Summary:     "Close a submission without publishing it",
		Tags:        []string{"moderation"},
		Metadata:    moderatorMetadata(ratelimit.TierSearch),
		Errors: []int{
			http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
			http.StatusConflict, http.StatusTooManyRequests,
		},
	}, func(ctx context.Context, in *DecisionInput) (*DecisionOutput, error) {
		user, ok := UserFromContext(ctx)
		if !ok {
			return nil, huma.Error403Forbidden("moderation requires a signed-in person")
		}
		if err := s.Moderation.Reject(ctx, store.Decision{
			SubmissionID: in.ID, ReviewerID: user.ID, Notes: in.Body.Notes,
		}); err != nil {
			return nil, mapModerationError(err)
		}

		out := &DecisionOutput{}
		out.Body.SubmissionID = in.ID
		out.Body.Status = model.SubmissionStatusRejected
		return out, nil
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "bulk-decide-submissions",
		Method:      http.MethodPost,
		Path:        "/api/v1/admin/submissions/bulk",
		Summary:     "Approve or reject many submissions in one call",
		Description: "Each decision is independent: one failure does not roll back the others, and every outcome is reported.",
		Tags:        []string{"moderation"},
		Metadata:    moderatorMetadata(ratelimit.TierSearch),
		Errors: []int{
			http.StatusUnauthorized, http.StatusForbidden,
			http.StatusUnprocessableEntity, http.StatusTooManyRequests,
		},
	}, func(ctx context.Context, in *BulkDecisionInput) (*BulkDecisionOutput, error) {
		user, ok := UserFromContext(ctx)
		if !ok {
			return nil, huma.Error403Forbidden("moderation requires a signed-in person")
		}
		if len(in.Body.IDs) > maxBulkDecisions {
			return nil, huma.Error422UnprocessableEntity(
				fmt.Sprintf("%d submissions in one request; the limit is %d", len(in.Body.IDs), maxBulkDecisions))
		}

		out := &BulkDecisionOutput{}
		out.Body.Outcomes = make([]BulkOutcome, 0, len(in.Body.IDs))
		for _, id := range in.Body.IDs {
			d := store.Decision{SubmissionID: id, ReviewerID: user.ID, Notes: in.Body.Notes}
			var err error
			if in.Body.Action == "approve" {
				_, err = s.Moderation.Approve(ctx, d)
			} else {
				err = s.Moderation.Reject(ctx, d)
			}

			// One bad id must not discard the other 99 decisions. A moderator
			// working a queue would otherwise have to bisect their own batch
			// to find which row poisoned it.
			outcome := BulkOutcome{SubmissionID: id, OK: err == nil}
			if err != nil {
				outcome.Error = bulkErrorMessage(err)
				out.Body.Failed++
			} else {
				out.Body.Succeeded++
			}
			out.Body.Outcomes = append(out.Body.Outcomes, outcome)
		}

		// Once for the batch rather than once per item: rebuilding is a full
		// table scan, and doing it fifty times for one bulk approval would
		// turn a convenience into the most expensive endpoint here. Only
		// approvals publish anything; a bulk rejection changes nothing public.
		if out.Body.Succeeded > 0 && in.Body.Action == "approve" {
			invalidatePublished()
		}
		return out, nil
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "revert-exercise",
		Method:      http.MethodPost,
		Path:        "/api/v1/admin/exercises/{id}/revert/{revision_id}",
		Summary:     "Make an earlier revision current again",
		Description: "One pointer write. Nothing is mutated or deleted, so a revert is itself revertible.",
		Tags:        []string{"moderation"},
		Metadata:    moderatorMetadata(ratelimit.TierSearch),
		Errors: []int{
			http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
			http.StatusUnprocessableEntity, http.StatusTooManyRequests,
		},
	}, func(ctx context.Context, in *RevertInput) (*RevertOutput, error) {
		if err := s.Moderation.Revert(ctx, in.ID, in.RevisionID); err != nil {
			if errors.Is(err, store.ErrRevisionNotOnExercise) {
				return nil, huma.Error422UnprocessableEntity("that revision belongs to a different exercise")
			}
			return nil, mapModerationError(err)
		}
		// A revert changes what /exercises returns, so the dump has to follow.
		invalidatePublished()

		out := &RevertOutput{}
		out.Body.ExerciseID = in.ID
		out.Body.RevisionID = in.RevisionID
		return out, nil
	})
}

// mapModerationError turns the store's sentinels into statuses a client can
// act on.
func mapModerationError(err error) error {
	switch {
	case errors.Is(err, store.ErrNoSuchSubmission), errors.Is(err, pgx.ErrNoRows):
		return huma.Error404NotFound("no such submission")
	case errors.Is(err, store.ErrAlreadyDecided):
		// 409, not 404 and not 200. The row exists and the caller's intent
		// was refused because someone got there first — which is exactly what
		// Conflict means, and what tells a second moderator to reload rather
		// than retry.
		return huma.Error409Conflict(err.Error())
	}
	return mapStoreError(err, "no such submission")
}

func bulkErrorMessage(err error) string {
	switch {
	case errors.Is(err, store.ErrNoSuchSubmission):
		return "no such submission"
	case errors.Is(err, store.ErrAlreadyDecided):
		return "already reviewed"
	}
	// Anything else is a server-side problem; the detail belongs in the log,
	// not in a response body that a moderator cannot act on.
	return "could not be processed"
}

func toQueued(s model.Submission) QueuedSubmission {
	return QueuedSubmission{
		Submission:     toContractSubmission(s),
		SubmittedByApp: s.SubmittedByApp,
		SubmittedBy:    s.SubmittedBy,
		ReviewerID:     s.ReviewerID,
		ReviewerNotes:  s.ReviewerNotes,
	}
}

// diffAgainstCurrent reports which fields a submission would change.
//
// Only fields the payload actually carries are compared. A payload omitting
// `level` is proposing nothing about level, and showing it as "level: beginner
// → null" would invent a change the submitter never made — which is worse than
// no diff at all, because a moderator would reject a good edit over it.
func diffAgainstCurrent(current model.Exercise, payload json.RawMessage) []FieldDiff {
	var proposed map[string]any
	if err := json.Unmarshal(payload, &proposed); err != nil {
		return []FieldDiff{}
	}

	// The content lives on the current revision, not the exercise row: an
	// exercise is an identity plus a pointer at whichever revision is live.
	rev := current.CurrentRevision
	if rev == nil {
		return []FieldDiff{}
	}
	currentByField := map[string]any{
		"name":              rev.Name,
		"muscle_group":      rev.MuscleGroup,
		"category":          rev.Category,
		"equipment":         rev.Equipment,
		"instructions":      rev.Instructions,
		"aliases":           rev.Aliases,
		"secondary_muscles": rev.SecondaryMuscles,
		"mechanic":          derefStr(rev.Mechanic),
		"force":             derefStr(rev.Force),
		"level":             derefStr(rev.Level),
	}

	// Stable order, so a moderator reading two diffs is comparing the same
	// shape rather than whatever order a map iterated in.
	order := []string{
		"name", "muscle_group", "category", "equipment", "mechanic", "force",
		"level", "instructions", "aliases", "secondary_muscles",
	}

	out := []FieldDiff{}
	for _, field := range order {
		p, sent := proposed[field]
		if !sent {
			continue
		}
		c := currentByField[field]
		if sameJSON(c, p) {
			continue
		}
		out = append(out, FieldDiff{Field: field, Current: c, Proposed: p})
	}
	return out
}

// sameJSON compares two values by their JSON encoding.
//
// Necessary because the current value is typed ([]string, string) while the
// proposed one came out of a map[string]any as []any — reflect.DeepEqual
// reports those as different even when they hold identical content, which
// would show every array field as changed on every diff.
func sameJSON(a, b any) bool {
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ab) == string(bb)
}

func derefStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
