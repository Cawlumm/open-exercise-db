-- Initial schema for open-exercise-db, per docs/DESIGN.md's ER diagram.
--
-- Table order is constrained by foreign keys: taxonomy_terms ->
-- taxonomy_term_translations -> users -> exercise_images -> exercise_videos ->
-- exercises -> exercise_revisions -> (ALTER exercises to add the
-- current_revision_id FK) -> exercise_translations -> submissions.
--
-- gen_random_uuid() is built into Postgres 13+ (pgcrypto is not required).

-- taxonomy_terms / taxonomy_term_translations
--
-- muscle_group / equipment / category / mechanic / force / level on
-- exercise_revisions are stored as plain TEXT slugs here, not UUID FKs into
-- taxonomy_terms. A composite FK on (facet, slug) is awkward in Postgres, and
-- TEXT slugs keep /export trivially matching lyftr's string-valued Exercise
-- shape (docs/DESIGN.md's "Migrating a consumer" section). The tradeoff is
-- that referential validity is enforced in the app layer against the seeded
-- taxonomy, not by the database.
CREATE TABLE taxonomy_terms (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    facet TEXT NOT NULL CHECK (facet IN ('muscle_group', 'equipment', 'category', 'mechanic', 'force', 'level')),
    slug TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Scoped to the facet, not global: a slug is only meaningful within its
    -- facet, and generic values legitimately recur across facets ('other',
    -- 'none', 'body-weight'). A global UNIQUE(slug) would let whichever facet
    -- was seeded first permanently claim the word.
    UNIQUE (facet, slug)
);

CREATE TABLE taxonomy_term_translations (
    term_id UUID NOT NULL REFERENCES taxonomy_terms (id) ON DELETE CASCADE,
    locale TEXT NOT NULL,
    label TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (term_id, locale)
);

CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    github_id TEXT NOT NULL UNIQUE,
    username TEXT NOT NULL,
    avatar_url TEXT,
    role TEXT NOT NULL DEFAULT 'contributor' CHECK (role IN ('contributor', 'moderator', 'admin')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE exercise_images (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    storage_key TEXT NOT NULL,
    content_type TEXT NOT NULL,
    width INT,
    height INT,
    license TEXT,
    attribution_text TEXT,
    -- Nullable: the Day-1 free-exercise-db bootstrap re-hosts images with no
    -- submitting user, mirroring exercise_revisions.submitted_by=NULL.
    uploaded_by UUID REFERENCES users (id),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE exercise_videos (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    storage_key TEXT,
    url TEXT,
    duration_seconds INT,
    thumbnail_image_id UUID REFERENCES exercise_images (id),
    license TEXT,
    attribution_text TEXT,
    uploaded_by UUID REFERENCES users (id),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A video is either hosted (own storage) or linked (e.g. YouTube).
    CONSTRAINT exercise_videos_storage_key_or_url CHECK (storage_key IS NOT NULL OR url IS NOT NULL)
);

-- current_revision_id has no FK yet: exercise_revisions doesn't exist until
-- the next CREATE TABLE below. The FK is added by ALTER TABLE afterward,
-- DEFERRABLE INITIALLY DEFERRED, so a single transaction can insert an
-- exercise row and its first revision together and only pay the constraint
-- at COMMIT.
CREATE TABLE exercises (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug TEXT NOT NULL UNIQUE,
    -- Upstream identity (e.g. free-exercise-db's own id), used only for
    -- import idempotency. Not to be confused with a consumer's own
    -- source_id column that points back at this table's id.
    source_id TEXT UNIQUE,
    current_revision_id UUID,
    status TEXT NOT NULL DEFAULT 'published' CHECK (status IN ('published', 'archived')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE exercise_revisions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    exercise_id UUID NOT NULL REFERENCES exercises (id),
    name TEXT NOT NULL,
    aliases TEXT[] NOT NULL DEFAULT '{}',
    -- See the taxonomy_terms comment above: these facet columns are slugs
    -- validated against taxonomy_terms in the app layer, not FKs.
    muscle_group TEXT NOT NULL,
    secondary_muscles TEXT[] NOT NULL DEFAULT '{}',
    category TEXT NOT NULL,
    equipment TEXT NOT NULL,
    mechanic TEXT,
    force TEXT,
    level TEXT,
    instructions JSONB NOT NULL DEFAULT '[]',
    image_ids UUID[] NOT NULL DEFAULT '{}',
    video_ids UUID[] NOT NULL DEFAULT '{}',
    license TEXT,
    attribution_text TEXT,
    source_url TEXT,
    submitted_by UUID REFERENCES users (id),
    reviewed_by UUID REFERENCES users (id),
    change_summary TEXT,
    status TEXT NOT NULL DEFAULT 'approved' CHECK (status IN ('approved', 'superseded')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE exercises
    ADD CONSTRAINT exercises_current_revision_id_fkey
    FOREIGN KEY (current_revision_id) REFERENCES exercise_revisions (id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE exercise_translations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    revision_id UUID NOT NULL REFERENCES exercise_revisions (id),
    locale TEXT NOT NULL,
    name TEXT NOT NULL,
    aliases TEXT[] NOT NULL DEFAULT '{}',
    instructions JSONB NOT NULL DEFAULT '[]',
    translated_by UUID REFERENCES users (id),
    origin TEXT NOT NULL DEFAULT 'human' CHECK (origin IN ('human', 'machine')),
    status TEXT NOT NULL DEFAULT 'approved' CHECK (status IN ('approved', 'superseded')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE submissions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    type TEXT NOT NULL CHECK (type IN ('create', 'edit', 'translation')),
    target_exercise_id UUID REFERENCES exercises (id),
    locale TEXT,
    payload JSONB NOT NULL,
    image_upload_ids UUID[] NOT NULL DEFAULT '{}',
    video_upload_ids UUID[] NOT NULL DEFAULT '{}',
    submitted_by UUID NOT NULL REFERENCES users (id),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'needs_changes')),
    reviewer_id UUID REFERENCES users (id),
    reviewer_notes TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- An edit or translation submission must target an existing exercise;
    -- only 'create' is allowed to omit it.
    CONSTRAINT submissions_target_exercise_required CHECK (type = 'create' OR target_exercise_id IS NOT NULL),
    -- A translation submission must declare which locale it's translating.
    CONSTRAINT submissions_locale_required CHECK (type != 'translation' OR locale IS NOT NULL)
);

-- Indexes. exercises.slug and the exercise_translations partial unique index
-- are already covered by their table's UNIQUE constraint/definition above.
CREATE INDEX idx_exercises_status ON exercises (status);
CREATE INDEX idx_exercise_revisions_exercise_id_status ON exercise_revisions (exercise_id, status);
CREATE INDEX idx_exercise_revisions_muscle_group ON exercise_revisions (muscle_group);
CREATE INDEX idx_exercise_revisions_equipment ON exercise_revisions (equipment);
CREATE INDEX idx_exercise_revisions_category ON exercise_revisions (category);
CREATE INDEX idx_submissions_status ON submissions (status);
CREATE INDEX idx_exercise_images_status ON exercise_images (status);

-- Partial unique index: superseded translation rows accumulate (revision
-- history is append-only), so uniqueness only applies to the live row per
-- (revision_id, locale).
CREATE UNIQUE INDEX idx_exercise_translations_revision_locale_approved
    ON exercise_translations (revision_id, locale)
    WHERE status = 'approved';
