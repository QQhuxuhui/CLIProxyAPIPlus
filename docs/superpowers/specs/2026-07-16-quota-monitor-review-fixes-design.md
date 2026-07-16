# Quota Monitor Review Fixes Design

## Scope

Fix the four defects found in the quota monitor review without changing route
paths, the current page workflow, or the executor/translator architecture.

## Antigravity Credits Refresh

The management handler's existing raw JSON literal has been byte-verified as
valid and will remain covered by an outbound request regression test. The
handler will use the request context without an `http.Client` timeout. OAuth
token acquisition retains its existing credential-acquisition timeout.

Credits response handling will match the executor semantics:

- A valid `GOOGLE_ONE_AI` entry produces a known hint with parsed balances.
- A response without an `availableCredits` array produces a known unavailable
  hint while preserving the paid tier.
- An array that contains no valid `GOOGLE_ONE_AI` entry does not overwrite the
  routing hint. The endpoint may still return the paid tier to the page.

The parser will return whether a routing hint is safe to cache, keeping response
parsing independently testable.

## Management Page Assets

The quota monitor will not load executable code or styles from a third-party
origin. The current layout will be preserved with CSS embedded in the existing
HTML asset, avoiding a new frontend build dependency and keeping the page usable
offline.

## Verification

Tests will cover the exact outbound JSON body, valid credits parsing, missing
credits, unmatched or malformed credit entries, absence of a client timeout, and
absence of remote script tags in the embedded page. The final verification set
is formatting, focused tests, all repository tests, race testing for the touched
management package, `go vet`, JavaScript syntax checking, and the required server
compile command.
