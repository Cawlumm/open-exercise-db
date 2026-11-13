# Error types

Every error response is an [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) problem document
whose `type` is a link to a section of this page. Switch on `type`, not on `title` or `detail`:
the type URIs are stable, the wording is not. A self-hosted instance returns the same URIs.

## not-found

HTTP 404. The addressed resource does not exist (an unknown exercise id, image id or route).

## validation-failed

HTTP 400 or 422. A request parameter or body is malformed or out of range. `errors` lists each
offending field and why.

## rate-limited

HTTP 429. The request exceeded a rate limit. Wait for `Retry-After` seconds; the
`RateLimit-*` headers say how much budget is left. Limits and how to request more are in
[API_POLICY.md](API_POLICY.md).

## method-not-allowed

HTTP 405. The path exists but does not accept this method.

## unauthorized

HTTP 401. The app credential or session is missing or unusable. Retrying with the same
credential will not succeed; check or rotate it.

## forbidden

HTTP 403. The credential is valid but not allowed to do this (today, a suspended app).
Rotating the credential changes nothing.

## internal

HTTP 5xx. A server-side failure. The response carries no detail on purpose; retry later, and
open an issue if it persists.
