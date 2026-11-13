# Changelog

All notable changes to the open-exercise-db API are recorded here. The format follows
[Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/) and the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); what a version means here is in
[`docs/VERSIONING.md`](docs/VERSIONING.md).

## [Unreleased]

## [1.0.0] - 2026-11-13

The first versioned release: the public read API, the static snapshot, app-submitted
contributions with moderation, and the policy that governs them.

### Read API

- `GET /api/v1/exercises` lists and searches exercises (`q`, `slug`, `muscle_group`, `equipment`,
  `category`, `mechanic`, `level`, `locale`, `page`, `per_page`). Every taxonomy filter takes
  several values (comma-separated or repeated); `ids` and `source_ids` fetch a known set of up
  to 100.
- `GET /api/v1/exercises/{id}` returns one exercise, `GET /api/v1/exercises/{id}/revisions` its
  append-only revision history, and `GET /api/v1/exercises/{id}/translations` the locales it is
  translated into. English and Spanish (`es`, machine-drafted and marked as such) are available.
- `GET /api/v1/exercises/export` returns the whole catalog in one response.
- `GET /api/v1/meta/muscle-groups`, `/equipment`, `/categories` and `/locales` list the
  taxonomy terms the filters accept.
- Exercise images are served at `GET /api/v1/images/{id}` (and `/{variant}`; only the `full`
  variant exists today), long-cached and usable from an `<img>` tag.
- Responses carry `ETag` and `Cache-Control`, and conditional requests answer `304`.
- OpenAPI 3.1 document at `/api/v1/openapi.json`, interactive docs at `/docs`, and a discovery
  index at `/api/v1`.

### Static snapshot

- The full catalog is published as static files, rebuilt every 6 hours, at
  `https://cawlumm.github.io/open-exercise-db/v1/manifest.json` (fallback: the `catalog` branch).
  Per-locale files are named by content hash, the manifest lists each file's `sha256`, and
  `schema_version` is `1`.

### Contributions

- Registered apps authenticate with an app credential and queue new exercises, edits and
  translations on their users' behalf (`POST /api/v1/submissions`,
  `GET /api/v1/submissions/mine`). Registered apps also get their own, larger rate-limit tier.
- Image uploads (`POST /api/v1/images`) are held behind the same review as text.
- Maintainers sign in with GitHub (`/api/v1/auth/github`, `/api/v1/me`) to register and manage
  apps, and to approve, reject, bulk-process and revert submissions
  (`/api/v1/admin/...`). Nothing unreviewed is published, and every published change can be
  reverted.

### Operations

- Rate limits are tiered by cost (app, image, read, search, export, unidentified) and answer `429`
  with `Retry-After` and `RateLimit-*` headers. See [`docs/API_POLICY.md`](docs/API_POLICY.md).
- Requests identify themselves with a `User-Agent` of the form `AppName/Version (Contact)`, or
  `X-OEDB-Client` from a browser; unidentified requests get a reduced budget rather than a
  rejection.
- Errors are RFC 9457 `application/problem+json` documents whose `type` links to
  [`docs/ERRORS.md`](docs/ERRORS.md).
- CORS is enabled for browser clients, and responses are compressed.
- `GET /health` reports `status` and the running `version`.

[Unreleased]: https://github.com/Cawlumm/open-exercise-db/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/Cawlumm/open-exercise-db/releases/tag/v1.0.0
