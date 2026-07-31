package export

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/Cawlumm/open-exercise-db/internal/contract"
	"github.com/Cawlumm/open-exercise-db/internal/model"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// Body is the top-level shape of the export blob. Exercises is
// []contract.Exercise — the exact same Go type internal/api uses for GET
// /exercises and GET /exercises/{id}, not a structurally-identical copy.
// See internal/contract/doc.go for why that had to be one type rather than
// two: two independently declared Go types both named "Exercise" panicked
// huma's schema registry the moment both were registered on one huma.API,
// which a JSON key-set-equality test (internal/api/export_test.go) could not
// catch, because it never exercised huma registration at all.
type Body struct {
	Exercises []contract.Exercise `json:"exercises"`
	Total     int                 `json:"total" doc:"Number of exercises in this export"`
	// GeneratedAt is when this blob was built, not when it was served — it
	// only changes on an actual rebuild, so consumers can tell a fresh mirror
	// from a stale one without diffing the whole payload. Deliberately
	// excluded from the ETag computation (see etagFor) so it doesn't churn
	// the ETag on a TTL-triggered rebuild that finds unchanged data.
	GeneratedAt time.Time `json:"generated_at"`
}

// Blob is one built-and-cached export: the response body plus its
// precomputed ETag. Computing the tag here, once, at build time — rather
// than per request the way internal/api.etagValue does for the small
// single-item/single-page endpoints — is the entire point of WO-5's caching
// requirement: this is the heaviest endpoint (docs/DESIGN.md -> Performance),
// so its cost has to be paid once per rebuild, not once per request.
type Blob struct {
	Body    Body
	ETag    string // bare, unquoted — same convention as internal/api.etagValue
	BuiltAt time.Time
}

// DefaultCacheTTL bounds how stale a served blob can be without any
// explicit invalidation signal. See Cache's doc comment for why TTL is the
// deliberate Phase 1 choice.
const DefaultCacheTTL = 15 * time.Minute

// Cache holds the single in-process copy of the export blob and rebuilds it
// on demand. This is Phase 1's stand-in for the write-through-to-R2 design
// docs/DESIGN.md actually specifies (see doc.go) — everything below is
// scoped to "correct and cheap for one process," not "the finished
// architecture."
//
// Invalidation strategy and why: docs/DESIGN.md's real trigger is
// "regenerated once on every approved submission/revert" — a Phase 2
// concept, since submissions and moderation don't exist yet
// (docs/PHASE1_BUILD_PLAN.md's guardrails explicitly rule out building
// Phase 2 endpoints here). Within Phase 1, the only way the underlying data
// changes while a server process is already running is an operator
// re-running cmd/import-freeexercisedb against a live database — not the
// documented normal flow (migrate, import, then start the server), and not
// something this package can observe without coupling it to the importer
// (a Phase-2-shaped dependency for a Phase-1-only edge case). Given that, a
// bounded TTL is the honest choice: it caps worst-case staleness at
// DefaultCacheTTL without requiring any signal from a writer, and it costs
// nothing when nothing has changed (a rebuild that finds identical data
// still produces the identical ETag — see etagFor). Invalidate exists
// alongside the TTL specifically so Phase 2's approval flow has a call to
// make the moment it exists, without this type's shape changing then.
//
// Concurrency: Get takes the read lock to check freshness, then the write
// lock to rebuild — a second read-write lock check after acquiring the write
// lock avoids two goroutines racing to rebuild back-to-back right after the
// TTL expires.
type Cache struct {
	mu   sync.RWMutex
	blob *Blob
	ttl  time.Duration
}

// NewCache constructs a Cache with the given TTL. ttl <= 0 means "never
// expire once built" — used by tests that want full control over rebuilds
// via Invalidate rather than fighting a timer.
func NewCache(ttl time.Duration) *Cache {
	return &Cache{ttl: ttl}
}

// Get returns the current blob, rebuilding from s first if none has been
// built yet or the cached one has exceeded its TTL. Concurrent callers
// during a rebuild block on the write lock rather than triggering redundant
// rebuilds — for an ~800-row dataset a rebuild is milliseconds, so a brief
// stall is preferable to dogpiling Postgres.
func (c *Cache) Get(ctx context.Context, s *store.Store) (*Blob, error) {
	c.mu.RLock()
	blob := c.blob
	fresh := blob != nil && !c.expired(blob)
	c.mu.RUnlock()
	if fresh {
		return blob, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blob != nil && !c.expired(c.blob) {
		// Rebuilt by another goroutine while this one waited for the write
		// lock.
		return c.blob, nil
	}

	built, err := buildBlob(ctx, s)
	if err != nil {
		return nil, err
	}
	c.blob = built
	return built, nil
}

func (c *Cache) expired(blob *Blob) bool {
	return c.ttl > 0 && time.Since(blob.BuiltAt) >= c.ttl
}

// Invalidate forces the next Get to rebuild regardless of TTL. Not called
// from anywhere in Phase 1 — see Cache's doc comment — but tests use it to
// force a deterministic rebuild instead of racing a timer, and it's the
// hook Phase 2's submission-approval flow will call.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blob = nil
}

// buildBlob queries s for every published exercise and assembles the export
// body, then computes its ETag once. This is the one place per rebuild that
// touches Postgres — everything else is served from the cached result.
func buildBlob(ctx context.Context, s *store.Store) (*Blob, error) {
	exercises, err := s.Exercises.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("list exercises for export: %w", err)
	}

	body := Body{GeneratedAt: time.Now().UTC()}
	for _, ex := range exercises {
		assembled, err := assembleExercise(ctx, s, ex)
		if err != nil {
			return nil, err
		}
		body.Exercises = append(body.Exercises, assembled)
	}
	body.Total = len(body.Exercises)

	tag, err := etagFor(body)
	if err != nil {
		return nil, err
	}

	return &Blob{Body: body, ETag: tag, BuiltAt: time.Now()}, nil
}

// assembleExercise joins one store-layer exercise to its locale-resolved
// (English-default) translation content, producing the same contract.Exercise
// internal/api's resolveExercise (exercises.go) builds for GET /exercises —
// deliberately parallel logic, not a shared call, since the two callers
// start from different store shapes (a single-page slice here vs. the same
// per-item join there); the guarantee that matters is the *output* type
// being identical, which contract.Exercise is by construction now.
func assembleExercise(ctx context.Context, s *store.Store, ex model.Exercise) (contract.Exercise, error) {
	if ex.CurrentRevision == nil {
		// store.Exercises.ListAll always joins current_revision_id; nil here
		// is a store invariant violation, not a client-facing condition.
		return contract.Exercise{}, fmt.Errorf("exercise %s: current revision not populated", ex.ID)
	}
	rev := ex.CurrentRevision

	resolved, err := s.Translations.GetForRevision(ctx, rev.ID, "")
	if err != nil {
		return contract.Exercise{}, fmt.Errorf("resolve translation for exercise %s: %w", ex.ID, err)
	}

	return contract.Exercise{
		ID:               ex.ID,
		Slug:             ex.Slug,
		SourceID:         ex.SourceID,
		Status:           ex.Status,
		CreatedAt:        ex.CreatedAt,
		UpdatedAt:        ex.UpdatedAt,
		RevisionID:       rev.ID,
		MuscleGroup:      rev.MuscleGroup,
		SecondaryMuscles: rev.SecondaryMuscles,
		Category:         rev.Category,
		Equipment:        rev.Equipment,
		Mechanic:         rev.Mechanic,
		Force:            rev.Force,
		Level:            rev.Level,
		ImageIDs:         rev.ImageIDs,
		VideoIDs:         rev.VideoIDs,
		License:          rev.License,
		AttributionText:  rev.AttributionText,
		SourceURL:        rev.SourceURL,
		Translation: contract.Translation{
			RequestedLocale:           resolved.RequestedLocale,
			Name:                      resolved.Name,
			NameLocale:                resolved.NameLocale,
			NameMachineOrigin:         resolved.NameMachineOrigin,
			Aliases:                   resolved.Aliases,
			AliasesLocale:             resolved.AliasesLocale,
			AliasesMachineOrigin:      resolved.AliasesMachineOrigin,
			Instructions:              resolved.Instructions,
			InstructionsLocale:        resolved.InstructionsLocale,
			InstructionsMachineOrigin: resolved.InstructionsMachineOrigin,
		},
	}, nil
}

// etagPayload is what actually gets hashed for the blob's ETag: the content
// consumers care about, not build metadata. Body.GeneratedAt is deliberately
// excluded — including it would change the ETag on every TTL-triggered
// rebuild even when the underlying data is byte-identical, which would make
// every mirror re-download the full catalog every DefaultCacheTTL for
// nothing.
type etagPayload struct {
	Exercises []contract.Exercise `json:"exercises"`
	Total     int                 `json:"total"`
}

// etagFor computes a strong-comparison-capable content hash, same
// bare/unquoted convention as internal/api.etagValue (see that function's
// comment for the quoting rationale — internal/api.etagHeader quotes it for
// the wire). Duplicated rather than called because this package cannot
// import internal/api (api.go's etagValue is unexported, and even if it
// weren't, internal/api already imports internal/export — see
// internal/contract/doc.go).
func etagFor(body Body) (string, error) {
	b, err := json.Marshal(etagPayload{Exercises: body.Exercises, Total: body.Total})
	if err != nil {
		return "", fmt.Errorf("marshal export body for etag: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16]), nil
}
