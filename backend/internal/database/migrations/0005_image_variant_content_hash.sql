-- Record each rendition's content hash, so the image routes can answer a
-- conditional GET without reading the object.
--
-- Until now the ETag on GET /api/v1/images/{id} was computed by downloading
-- the whole object from the media store and hashing it, and only then
-- comparing If-None-Match. A 304 therefore cost the same storage read as a
-- 200, minus egress. With the hash recorded here the handler compares
-- against the row it already fetched and never touches the bucket.
--
-- Nullable, and NULL means "unknown": the handler falls back to reading and
-- hashing the bytes, exactly as before. That is also the safe state for any
-- writer that does not know the hash — store.UpsertImageVariant writes
-- whatever the caller passes, so a writer that replaces bytes without
-- stating a hash clears it rather than leaving a stale one behind.
--
-- Existing rows start NULL. cmd/backfill-thumbs fills them as it reads each
-- full image to make its thumbnail; no hash is invented here, because the
-- only honest source of one is the bytes themselves.
--
-- sha256 as 64 lowercase hex characters. The API's ETag is the first 32 of
-- them, which is the same value etagValueBytes produced from the bytes, so
-- a client's cached ETag stays valid across this change.
ALTER TABLE exercise_image_variants
    ADD COLUMN content_sha256 TEXT
    CHECK (content_sha256 IS NULL OR content_sha256 ~ '^[0-9a-f]{64}$');
