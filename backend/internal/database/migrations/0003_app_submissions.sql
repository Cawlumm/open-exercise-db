-- Apps that write on behalf of their own users.
--
-- Until now a submission could only come from a person holding a GitHub
-- account (submissions.submitted_by NOT NULL REFERENCES users). That is the
-- right model for someone visiting this project's own UI, and the wrong one
-- for the way contributions actually arrive: Open Food Facts' data comes
-- overwhelmingly from third-party apps whose users never knowingly interact
-- with OFF at all. The first consumer here is lyftr, whose users are
-- gym-goers — requiring a GitHub account to fix a typo in an exercise
-- description is a wall almost none of them climb, and the failure is silent,
-- because the people who would have contributed simply close the screen.
--
-- See docs/DESIGN.md's auth section for the full reasoning, including why
-- adopting OFF's *auth* model deliberately does not mean adopting its
-- *publication* model: review still gates publication here.
CREATE TABLE apps (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Display identity. Shown next to submissions in the moderation queue, so
    -- a reviewer can weigh "lyftr sent this" without joining anything.
    name TEXT NOT NULL UNIQUE CHECK (length(trim(name)) > 0),
    contact_email TEXT NOT NULL CHECK (length(trim(contact_email)) > 0),

    -- Split credential: key_id travels on every request and identifies which
    -- row to check; key_hash is what the presented secret is compared against.
    -- The secret itself is never stored, which is why rotation below can be a
    -- column update rather than a new row.
    key_id TEXT NOT NULL UNIQUE CHECK (length(trim(key_id)) > 0),
    key_hash TEXT NOT NULL CHECK (length(trim(key_hash)) > 0),

    -- Who to talk to when an app misbehaves. Nullable: the first apps are
    -- registered by the maintainer out of band, before any self-service
    -- registration flow exists to attach an owner.
    owner_user_id UUID REFERENCES users (id),

    -- The blunt lever. Suspending stops everything from this source at once,
    -- and is deliberately separate from deleting the row: submissions already
    -- in the queue keep their attribution, and the suspension is reversible.
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),

    -- Per-app caps, not global constants. An app is the aggregate of many
    -- humans, so a limit sized for one contributor is wrong in both
    -- directions at once: it throttles a legitimate app down to one person's
    -- budget, while letting a thousand fabricated app_user_ref values through
    -- on a single credential. Columns rather than config because the right
    -- number for lyftr is not the right number for a hobby project's first
    -- integration.
    max_pending INT NOT NULL DEFAULT 50 CHECK (max_pending > 0),
    max_per_day INT NOT NULL DEFAULT 200 CHECK (max_per_day > 0),

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Every authenticated write starts by resolving the presented key_id to a row,
-- so this is the hot path for the entire write API. UNIQUE above already
-- provides the index; named here only to say that is deliberate and no second
-- index is needed.

-- A submission may now come from a person OR from an app, so the person is no
-- longer mandatory. Dropping NOT NULL rather than inventing a sentinel user
-- row: a fake "app user" in `users` would leak into every join, every count of
-- contributors, and every moderator-facing screen that assumes a user is a
-- person.
ALTER TABLE submissions ALTER COLUMN submitted_by DROP NOT NULL;

ALTER TABLE submissions ADD COLUMN submitted_by_app UUID REFERENCES apps (id);

-- Whatever opaque handle the app uses for the human behind this submission.
--
-- Deliberately NOT an identity in this system: no row, no email, no login, no
-- foreign key. It exists for exactly two jobs — blocking one bad contributor
-- inside an app without suspending the whole app, and letting an app answer
-- "what happened to my edit?" for its own user. Treating it as an identity
-- would drag this project into storing third-party user data it has no reason
-- to hold, and into the obligations that come with holding it.
ALTER TABLE submissions ADD COLUMN app_user_ref TEXT;

-- Exactly one attribution, enforced rather than assumed. "Who sent this" is
-- the input to every abuse control downstream — per-app caps, per-user caps,
-- blocking a single app_user_ref — and each of those silently does nothing on
-- a row attributed to neither. A NULL-NULL submission would sail past every
-- limit while looking like a normal pending row in the queue.
ALTER TABLE submissions ADD CONSTRAINT submissions_one_attribution CHECK (
    (submitted_by IS NOT NULL AND submitted_by_app IS NULL)
    OR (submitted_by IS NULL AND submitted_by_app IS NOT NULL)
);

-- app_user_ref belongs to app submissions and is meaningless without one.
-- Without this, a human submission carrying a stray app_user_ref would be
-- accepted and then counted by the per-app-user blocking logic against an app
-- that never sent it.
--
-- Non-empty for the same reason every other text column on `apps` is: an
-- empty ref is not a contributor. Left unconstrained, an app could send
-- app_user_ref = '' on every submission and the per-contributor counter would
-- have nothing to key on — the rows are indistinguishable from each other, so
-- the cap can never fire and the only remaining lever is suspending the whole
-- app, which is precisely the outcome the two-lever design exists to avoid.
-- NULL stays legal: it means "no per-user attribution offered", which is a
-- different claim from "this user has no name".
ALTER TABLE submissions ADD CONSTRAINT submissions_app_user_ref_requires_app CHECK (
    app_user_ref IS NULL
    OR (submitted_by_app IS NOT NULL AND length(trim(app_user_ref)) > 0)
);

-- One index per cap query, each leading with submitted_by_app because every
-- such query is scoped to one app first.
--
-- These are only usable because store.AppStore.UsageFor expresses each count
-- as its own subquery with its own WHERE. An earlier draft wrote all three as
-- count(*) FILTER over a single scan, which Postgres evaluates per row after
-- the fact: the indexes existed, matched the comment, and pruned nothing,
-- leaving every cap check to read the app's entire submission history. If
-- that query is ever collapsed back into FILTER clauses, these become dead
-- weight rather than a speedup.
CREATE INDEX idx_submissions_app_status ON submissions (submitted_by_app, status);
CREATE INDEX idx_submissions_app_created_at ON submissions (submitted_by_app, created_at);

-- Blocking a single contributor inside an app is a lookup on the pair, not on
-- app_user_ref alone — the same ref string from two different apps refers to
-- two unrelated people, and must never collide. created_at is included
-- because the per-contributor count is windowed exactly like the app-wide
-- one, so this serves the whole predicate rather than half of it.
CREATE INDEX idx_submissions_app_user_ref ON submissions (submitted_by_app, app_user_ref, created_at)
    WHERE app_user_ref IS NOT NULL;
