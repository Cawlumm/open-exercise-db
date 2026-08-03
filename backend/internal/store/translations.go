package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/model"
)

// TranslationStore holds every query touching exercise_translations.
type TranslationStore struct {
	pool *pgxpool.Pool
}

// GetForRevision resolves revisionID's translatable content for locale,
// falling back to the revision's own (English) fields per field — not the
// whole record — when the locale's translation row is missing or has an
// empty field. Per docs/PHASE1_BUILD_PLAN.md's WO-2 locale fallback rule,
// this is deliberately field-by-field: a Spanish translation with a name
// but no instructions yields an English instructions field, not an English
// record.
//
// Returns pgx.ErrNoRows only when revisionID itself doesn't exist — a
// missing or partial translation is a fallback, not an error.
func (s *TranslationStore) GetForRevision(ctx context.Context, revisionID, locale string) (model.ResolvedTranslation, error) {
	var rev model.Revision
	err := s.pool.QueryRow(ctx, `
		SELECT name, aliases, instructions
		FROM exercise_revisions
		WHERE id = $1`, revisionID,
	).Scan(&rev.Name, &rev.Aliases, &rev.Instructions)
	if err != nil {
		return model.ResolvedTranslation{}, fmt.Errorf("get revision %s: %w", revisionID, err)
	}

	result := model.ResolvedTranslation{
		RequestedLocale:    locale,
		Name:               rev.Name,
		NameLocale:         englishLocale,
		Aliases:            rev.Aliases,
		AliasesLocale:      englishLocale,
		Instructions:       rev.Instructions,
		InstructionsLocale: englishLocale,
	}

	if locale == "" || locale == englishLocale {
		result.RequestedLocale = englishLocale
		return result, nil
	}

	var t model.Translation
	err = s.pool.QueryRow(ctx, `
		SELECT name, aliases, instructions, origin
		FROM exercise_translations
		WHERE revision_id = $1 AND locale = $2 AND status = 'approved'`,
		revisionID, locale,
	).Scan(&t.Name, &t.Aliases, &t.Instructions, &t.Origin)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No translation row at all for this locale: every field stays
			// on the English fallback already set above.
			return result, nil
		}
		return model.ResolvedTranslation{}, fmt.Errorf("get translation for revision %s locale %s: %w", revisionID, locale, err)
	}

	machineOrigin := t.Origin == "machine"

	if t.Name != "" {
		result.Name = t.Name
		result.NameLocale = locale
		result.NameMachineOrigin = machineOrigin
	}
	if len(t.Aliases) > 0 {
		result.Aliases = t.Aliases
		result.AliasesLocale = locale
		result.AliasesMachineOrigin = machineOrigin
	}
	if len(t.Instructions) > 0 {
		result.Instructions = t.Instructions
		result.InstructionsLocale = locale
		result.InstructionsMachineOrigin = machineOrigin
	}

	return result, nil
}

// ListLocalesForExercise returns one summary per locale that has an
// approved translation of exerciseID's *current* revision — not any
// revision, since a translation of an older superseded revision isn't
// something a consumer of the live exercise needs to see. Complete means
// the translation row itself supplies a non-empty name and a non-empty
// instructions array; it does not consider English fallback, since
// completeness is about how much of that locale's own translation exists.
// CountLocalesForExercise returns how many locales have an approved
// translation of exerciseID's current revision, ignoring pagination. The
// WHERE clause is kept identical to ListLocalesForExercise's below; if the
// two ever diverge the reported Total stops describing the page it is
// attached to.
func (s *TranslationStore) CountLocalesForExercise(ctx context.Context, exerciseID string) (int, error) {
	var total int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM exercise_translations t
		JOIN exercises e ON e.current_revision_id = t.revision_id
		WHERE e.id = $1 AND t.status = 'approved'`, exerciseID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("count translation locales for exercise %s: %w", exerciseID, err)
	}
	return total, nil
}

func (s *TranslationStore) ListLocalesForExercise(ctx context.Context, exerciseID string, limit, offset int) ([]model.TranslationSummary, error) {
	// t.locale is unique per revision, so ORDER BY locale is deterministic
	// across pages without a tiebreak column.
	rows, err := s.pool.Query(ctx, `
		SELECT t.locale, t.origin, t.revision_id,
		       (t.name <> '' AND jsonb_array_length(t.instructions) > 0) AS complete
		FROM exercise_translations t
		JOIN exercises e ON e.current_revision_id = t.revision_id
		WHERE e.id = $1 AND t.status = 'approved'
		ORDER BY t.locale ASC
		LIMIT $2 OFFSET $3`, exerciseID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list translation locales for exercise %s: %w", exerciseID, err)
	}
	defer rows.Close()

	var summaries []model.TranslationSummary
	for rows.Next() {
		var sum model.TranslationSummary
		if err := rows.Scan(&sum.Locale, &sum.Origin, &sum.RevisionID, &sum.Complete); err != nil {
			return nil, fmt.Errorf("scan translation summary: %w", err)
		}
		summaries = append(summaries, sum)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list translation locales for exercise %s: %w", exerciseID, err)
	}

	return summaries, nil
}
