package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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

	// Match the exact locale first, then its base language: a caller asking for
	// es-MX should get Spanish rather than English when only "es" is
	// translated, which is the whole point of a region subtag being a
	// refinement rather than a different language. Without this the exact-match
	// lookup missed and every regional locale fell all the way back to English,
	// silently — es-MX, pt-BR and en-GB are the forms real Accept-Language
	// headers actually carry, so the common case was the broken one.
	//
	// ORDER BY puts an exact match ahead of the base-language row so the more
	// specific translation always wins when both exist; LIMIT 1 then takes it.
	var servedLocale string
	var t model.Translation
	err = s.pool.QueryRow(ctx, `
		SELECT name, aliases, instructions, origin, locale
		FROM exercise_translations
		WHERE revision_id = $1 AND status = 'approved' AND locale IN ($2, $3)
		ORDER BY (locale = $2) DESC
		LIMIT 1`,
		revisionID, locale, baseLanguage(locale),
	).Scan(&t.Name, &t.Aliases, &t.Instructions, &t.Origin, &servedLocale)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No translation row at all for this locale: every field stays
			// on the English fallback already set above.
			return result, nil
		}
		return model.ResolvedTranslation{}, fmt.Errorf("get translation for revision %s locale %s: %w", revisionID, locale, err)
	}

	machineOrigin := t.Origin == "machine"

	// servedLocale, not the requested one: with base-language fallback these
	// legitimately differ (asked es-MX, served es), and the per-field *_locale
	// fields exist precisely so a client can see which. Reporting the requested
	// locale here would claim a regional translation that does not exist.
	if t.Name != "" {
		result.Name = t.Name
		result.NameLocale = servedLocale
		result.NameMachineOrigin = machineOrigin
	}
	if len(t.Aliases) > 0 {
		result.Aliases = t.Aliases
		result.AliasesLocale = servedLocale
		result.AliasesMachineOrigin = machineOrigin
	}
	if len(t.Instructions) > 0 {
		result.Instructions = t.Instructions
		result.InstructionsLocale = servedLocale
		result.InstructionsMachineOrigin = machineOrigin
	}

	return result, nil
}

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

// ListLocalesForExercise returns one page of summaries, one per locale that
// has an approved translation of exerciseID's *current* revision — not any
// revision, since a translation of an older superseded revision isn't
// something a consumer of the live exercise needs to see. Complete means the
// translation row itself supplies a non-empty name and a non-empty
// instructions array; it does not consider English fallback, since
// completeness is about how much of that locale's own translation exists.
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

// baseLanguage returns the language subtag of a BCP-47 tag: "es-MX" -> "es".
// A tag with no region is its own base, and returning it unchanged makes the
// IN ($2, $3) lookup above degenerate to a plain exact match rather than
// needing a separate query for the two cases.
func baseLanguage(locale string) string {
	if i := strings.IndexByte(locale, '-'); i > 0 {
		return locale[:i]
	}
	return locale
}

// MachineTranslation is one exercise's draft translation, keyed by the stable
// source_id rather than a database id so one file imports into any deployment.
// EnglishName is the name the translation was written against; it is what lets
// an import notice that the exercise has since been edited.
type MachineTranslation struct {
	SourceID     string   `json:"source_id"`
	EnglishName  string   `json:"en_name"`
	Name         string   `json:"name"`
	Aliases      []string `json:"aliases"`
	Instructions []string `json:"instructions"`
}

// MachineImportResult counts what ImportMachine did with each item.
type MachineImportResult struct {
	Inserted   int
	Replaced   int // a previous machine draft for the revision was superseded
	Unchanged  int
	KeptHuman  int
	Stale      int // English name no longer matches: the exercise was edited
	Mismatched int // instruction step count differs from the English source
	NotFound   int
}

// ImportMachine writes items as origin='machine' translations of each
// exercise's current revision, in one transaction. It never overwrites a human
// translation, supersedes (rather than deletes) an older machine draft, and
// skips an item whose English name or step count no longer matches the
// revision, since a draft of text that has changed is wrong, not merely old.
// With dryRun the transaction is rolled back after counting.
func (s *TranslationStore) ImportMachine(ctx context.Context, locale string, items []MachineTranslation, dryRun bool) (MachineImportResult, error) {
	var res MachineImportResult
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin import tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	for _, it := range items {
		if strings.TrimSpace(it.Name) == "" {
			return res, fmt.Errorf("%s: translation has an empty name", it.SourceID)
		}
		var revisionID, name string
		var instructions []byte
		err := tx.QueryRow(ctx, `
			SELECT r.id, r.name, r.instructions
			FROM exercises e JOIN exercise_revisions r ON r.id = e.current_revision_id
			WHERE e.source_id = $1 AND e.status = 'published'`, it.SourceID,
		).Scan(&revisionID, &name, &instructions)
		if errors.Is(err, pgx.ErrNoRows) {
			res.NotFound++
			continue
		}
		if err != nil {
			return res, fmt.Errorf("%s: load current revision: %w", it.SourceID, err)
		}
		if name != it.EnglishName {
			res.Stale++
			continue
		}
		var steps []string
		if err := json.Unmarshal(instructions, &steps); err != nil {
			return res, fmt.Errorf("%s: decode English instructions: %w", it.SourceID, err)
		}
		if len(steps) != len(it.Instructions) {
			res.Mismatched++
			continue
		}

		encoded, err := json.Marshal(it.Instructions)
		if err != nil {
			return res, fmt.Errorf("%s: encode instructions: %w", it.SourceID, err)
		}
		aliases := it.Aliases
		if aliases == nil {
			aliases = []string{}
		}

		var origin, oldName string
		var oldAliases []string
		var oldInstr []byte
		err = tx.QueryRow(ctx, `
			SELECT origin, name, aliases, instructions FROM exercise_translations
			WHERE revision_id = $1 AND locale = $2 AND status = 'approved'`, revisionID, locale,
		).Scan(&origin, &oldName, &oldAliases, &oldInstr)
		replacing := false
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return res, fmt.Errorf("%s: load existing translation: %w", it.SourceID, err)
		case origin == "human":
			res.KeptHuman++
			continue
		default:
			var old []string
			_ = json.Unmarshal(oldInstr, &old)
			if oldName == it.Name && equalStrings(oldAliases, aliases) && equalStrings(old, it.Instructions) {
				res.Unchanged++
				continue
			}
			replacing = true
		}

		if replacing {
			if _, err := tx.Exec(ctx, `
				UPDATE exercise_translations SET status = 'superseded'
				WHERE revision_id = $1 AND locale = $2 AND status = 'approved'`, revisionID, locale); err != nil {
				return res, fmt.Errorf("%s: supersede machine draft: %w", it.SourceID, err)
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO exercise_translations
				(revision_id, locale, name, aliases, instructions, translated_by, origin, status)
			VALUES ($1, $2, $3, $4, $5, NULL, 'machine', 'approved')`,
			revisionID, locale, it.Name, aliases, encoded); err != nil {
			return res, fmt.Errorf("%s: insert machine translation: %w", it.SourceID, err)
		}
		if replacing {
			res.Replaced++
		} else {
			res.Inserted++
		}
	}

	if dryRun {
		return res, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit import: %w", err)
	}
	return res, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
