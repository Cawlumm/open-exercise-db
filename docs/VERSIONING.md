# Versioning

open-exercise-db releases follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html),
and the thing being versioned is the **HTTP API contract**: the paths, parameters, response
shapes and error types in the OpenAPI document.

| Bump | When | Example |
|---|---|---|
| **MAJOR** | A breaking change to the API: removing or renaming an endpoint, parameter or field, changing a type or meaning, tightening validation so a previously valid request fails | `2.0.0` |
| **MINOR** | An additive change: a new endpoint, optional parameter, response field or error type. Existing clients keep working | `1.1.0` |
| **PATCH** | A fix that changes no contract: a bug, a performance fix, a docs correction | `1.0.1` |

The `/api/v1` path prefix stays for every 1.x release. A 2.0.0 would be served under
`/api/v2`, so apps built against v1 are never broken in place. Clients should ignore response
fields they do not know about, since MINOR releases add them.

## What is not versioned by releases

- **The exercise data.** Exercises change all the time as submissions are approved, and a
  release does not snapshot them. Every exercise has its own append-only revision history
  (`GET /api/v1/exercises/{id}/revisions`).
- **The static snapshot's format** has its own `schema_version` in `manifest.json` (currently
  `1`). It is bumped only for a breaking change to the snapshot format, and independently of the
  API version. Check it before reading the files.

## Looking back at a version

- **What changed:** [`CHANGELOG.md`](../CHANGELOG.md), or the notes on each
  [GitHub release](https://github.com/Cawlumm/open-exercise-db/releases).
- **The API spec:** every release has that version's `openapi.json` attached.
- **The docs:** as of a version, at
  `https://github.com/Cawlumm/open-exercise-db/tree/vX.Y.Z/docs`.
- **What is running now:** `info.version` in `/api/v1/openapi.json`, or `version` in `/health`
  and at `/api/v1`. A build between releases reports `X.Y.Z-N-gSHA`; a local `go run` reports `dev`.

## Cutting a release (maintainers)

1. In a pull request, move the `## [Unreleased]` entries in `CHANGELOG.md` into a new
   `## [X.Y.Z] - YYYY-MM-DD` section, leave `[Unreleased]` empty, and update the link
   references at the bottom.
2. Merge it.
3. Tag the merge commit on `main` and push the tag:

   ```bash
   git switch main && git pull
   git tag vX.Y.Z && git push origin vX.Y.Z
   ```

The [release workflow](../.github/workflows/release.yml) checks that the tag is on `main` and
that the changelog has a section for it, builds `openapi.json` with the version injected, and
creates the GitHub release with the changelog section as its notes. Production deploys from
every merge to `main` and takes its version from `git describe`, so it reports `X.Y.Z` once the
tagged commit is what is live.
