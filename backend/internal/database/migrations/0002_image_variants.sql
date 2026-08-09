-- Image variants: make a storage key data rather than a derived convention.
--
-- Until now the key for an image was computed, not recorded —
-- internal/seed built "exercises/<source-id>/<index>.jpg" and every reader
-- re-derived the same string. That works exactly as long as there is one
-- rendition per image. The moment a second size exists, the convention has
-- to change shape, and changing a convention that 1,746 live objects were
-- named by means moving all of them: a bucket-wide copy, a window where
-- half the keys are old and half are new, and a rollback that has to move
-- them back.
--
-- Recording each rendition's key instead removes that entirely. Existing
-- objects keep the names they already have — this migration moves no bytes
-- and touches no bucket — and adding a 200w thumbnail later is an INSERT
-- naming whatever key it likes. There is no future migration where the
-- objects have to catch up with the schema, which is the whole point of
-- doing this before the production backfill rather than after.
--
-- Deliberately not a JSONB column on exercise_images. Variants are queried
-- by name and joined per image on the hot read path (GET /exercises renders
-- up to 2 images per exercise, 50 exercises per page), and they carry a
-- uniqueness rule a document can't express: one row per (image, name), and
-- no two images sharing a storage key.
CREATE TABLE exercise_image_variants (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    image_id UUID NOT NULL REFERENCES exercise_images (id) ON DELETE CASCADE,

    -- Rendition name: 'full' today, 'thumb'/'medium'/etc. later. Free text
    -- rather than a CHECK list or a taxonomy facet — unlike muscle groups,
    -- this vocabulary is decided by whoever runs the encoder, and pinning it
    -- in a CHECK would mean a migration to add a size, reintroducing exactly
    -- the coupling this table removes. Non-empty is the only real rule.
    name TEXT NOT NULL CHECK (length(trim(name)) > 0),

    -- The authoritative location of these bytes. UNIQUE because two rows
    -- claiming one object is a corruption that reads as success: deleting
    -- one variant would silently break the other.
    storage_key TEXT NOT NULL UNIQUE,

    -- Per-variant, not inherited: the whole reason a variant exists is that
    -- its dimensions differ, and a resized rendition may legitimately differ
    -- in format too (a 200w thumb as WebP while the full stays JPEG).
    content_type TEXT NOT NULL,
    width INT,
    height INT,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One rendition per name per image. This is what lets a re-encode be an
    -- upsert rather than a duplicate, mirroring the (facet, slug) rule on
    -- taxonomy_terms.
    CONSTRAINT exercise_image_variants_image_name_unique UNIQUE (image_id, name)
);

-- The read path is "give me every variant for these images", driven by a
-- revision's image_ids[]. image_id alone is the index that serves it; the
-- UNIQUE constraint above already covers (image_id, name) lookups.
CREATE INDEX idx_exercise_image_variants_image_id ON exercise_image_variants (image_id);

-- Backfill every existing image as its own 'full' variant, carrying the
-- storage_key it already has. This is the step that makes the whole change
-- free: after it, every object in R2 and on every self-hoster's disk is
-- already correctly described, at the name it was written under, without
-- anything having been copied or renamed.
--
-- Runs inside the migration's transaction (internal/database.applyMigration
-- wraps each file), so the table and its contents commit together — a
-- reader can never observe the table existing but empty.
INSERT INTO exercise_image_variants (image_id, name, storage_key, content_type, width, height)
SELECT id, 'full', storage_key, content_type, width, height
FROM exercise_images;
