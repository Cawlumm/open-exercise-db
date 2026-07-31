<!--
Scratch file, not part of this project's documentation set. Drafted as the
basis for an upstream bug report against github.com/danielgtaylor/huma. Not
linked from README.md, DESIGN.md, CONVENTIONS.md, or PHASE1_BUILD_PLAN.md —
delete or move once filed.
-->

# `conditional.Params.PreconditionFailed` rejects RFC 9110-legal `If-None-Match`/`If-Match` lists with OWS after a comma

## Version

`github.com/danielgtaylor/huma/v2 v2.39.1` (module cache path:
`github.com/danielgtaylor/huma/v2@v2.39.1`), specifically the
`conditional` subpackage combined with the core request-parameter parser
in `huma.go`.

## Summary

A multi-value `If-None-Match` (or `If-Match`) header whose elements are
separated by `", "` (comma + a space, or any other optional whitespace) —
which RFC 9110 §5.6.1's list grammar explicitly permits — fails to match
an ETag that would match if the same header were sent without the space.
This means `conditional.Params.PreconditionFailed` can silently miss a
legitimate conditional-request match for any list element after the
first, causing a 304-eligible request to instead return a full 200 with
the complete response body.

## Root cause

1. `huma.go`'s parameter parser splits a list-style header/query param on
   a literal comma with no whitespace handling:

   ```go
   // huma.go:1871 (parseInto, reflect.Slice case, non-Explode path)
   values = strings.Split(value, ",")
   ```

   For `If-None-Match: "a", "b"` this produces `[]string{`"a"`, ` "b"`}` —
   note the leading space preserved on the second element.

2. `conditional.trimETag` (`conditional/params.go:24-29`) is the only place
   that later tries to normalize an incoming list element for comparison,
   and it only strips a `W/` prefix and `"` characters:

   ```go
   // conditional/params.go:24-29
   func trimETag(value string) string {
       if strings.HasPrefix(value, "W/") && len(value) > 2 {
           value = value[2:]
       }
       return strings.Trim(value, "\"")
   }
   ```

   `strings.Trim(value, "\"")` only removes leading/trailing characters
   that are themselves `"`. Given ` "b"` (leading space), the leading
   character is a space, not `"`, so `Trim` stops immediately at the front
   and removes nothing from that side — the result is ` "b"` un-quoted at
   the front, `b"`... — a value that can never equal the bare tag `b` a
   caller passes into `PreconditionFailed`.

3. `PreconditionFailed` (`conditional/params.go:66-152`) then compares
   `trimETag(match) == etag` for each list element, so the space-tainted
   element never matches even though its "real" value is a legitimate,
   correctly-quoted entity-tag.

## RFC citation

RFC 9110 §5.6.1 defines the list extension used by `If-Match` (§13.1.1)
and `If-None-Match` (§13.1.2):

> `#element => [ element ] *( OWS "," OWS [ element ] )`

`OWS` (optional whitespace) is explicitly part of the grammar around each
comma. A client sending `If-None-Match: "a", "b"` — the conventional,
human-readable way to write a comma list, and what `net/http`'s own
`http.Header.Add`-then-join behavior and most intermediary proxies/CDNs
produce — is fully RFC-conformant. huma should not require callers to
omit that whitespace to get a correct match.

## Minimal reproduction

```go
package main

import (
	"fmt"
	"time"

	"github.com/danielgtaylor/huma/v2/conditional"
)

func main() {
	p := &conditional.Params{
		// Note the space after the comma — RFC 9110 §5.6.1 OWS.
		IfNoneMatch: []string{`"other-tag"`, ` "target-tag"`},
	}

	// A resource whose current ETag is "target-tag".
	err := p.PreconditionFailed("target-tag", time.Time{})
	if err != nil {
		fmt.Println("matched (304 expected):", err.GetStatus())
	} else {
		fmt.Println("did NOT match (200 will be served) — bug reproduced")
	}
}
```

Or, against any server built on `humago` + `conditional.Params` embedded
in an operation's input struct:

```
curl -i -H 'If-None-Match: "other-tag", "target-tag"' https://example/resource
```

## Expected vs. actual

| Request | Expected | Actual |
|---|---|---|
| `If-None-Match: "target-tag"` | 304 | 304 (correct) |
| `If-None-Match: "other-tag","target-tag"` (no space) | 304 | 304 (correct) |
| `If-None-Match: "other-tag", "target-tag"` (RFC-legal space) | 304 | **200** |
| `If-None-Match: "target-tag", "other-tag"` (space lands on the *other* element) | 304 | 304 (correct, but only by luck of position) |

Only the first list element is ever compared reliably; every subsequent
element is one un-trimmed leading space away from silently failing to
match.

## Suggested fix

Trim OWS (`" \t"`) from each element after splitting, either at the split
site or inside `trimETag`. Smallest fix, at the split site in
`huma.go`'s `parseInto`:

```go
// huma.go:1871
values = strings.Split(value, ",")
for i, v := range values {
    values[i] = strings.Trim(v, " \t") // RFC 9110 §5.6.1 OWS
}
```

This is a general fix for every list-style header/query param huma parses
(not `conditional`-specific), which is probably the more correct place
for it — `conditional.trimETag` only ever sees what `parseInto` already
handed it, so fixing the split site fixes every caller at once rather
than just this one package.

## Workaround in use downstream

Since forking or vendoring huma isn't an option for this project, the
caller-side workaround is to trim each `IfMatch`/`IfNoneMatch` element to
`strings.Trim(v, " \t")` before calling `PreconditionFailed`, i.e.
normalize `conditional.Params`' own fields in place. See
`backend/internal/api/api.go`'s `checkNotModified`/`trimListOWS` in the
`open-exercise-db` repo (not part of this bug report, referenced only for
context) for the exact shape of the workaround.
