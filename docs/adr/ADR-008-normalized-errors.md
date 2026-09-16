# ADR-008: Normalized errors

Status: Accepted

Date: 2026-09-16

## Context

BSYSTEM sits between callers and systems it does not control. Those systems
fail in their own vocabularies: EspoCRM returns an HTML error page, Redmine a
404 for both "no such project" and "your key is wrong", Outline a 200 with an
error envelope inside it.

Two things go wrong if those failures are passed through.

The first is **disclosure**. An upstream error body routinely contains internal
hostnames, the failing URL, fragments of the record, and occasionally the
credential that was rejected. Forwarding it hands a caller a map of the
platform's internals in exchange for nothing they can act on.

The second is **actionability**. "500" from an upstream, "500" from the
platform's own database and "500" because a connection timed out are three
different situations for whoever is reading. Collapsing them tells a user to
retry when they should ask for access, or to wait when nothing will change.

## Decision

Every failure is rendered in one shape:

```json
{ "error": "...", "code": "...", "source": "...", "request_id": "..." }
```

`error` is always present and human-readable. `code` is the stable
machine-readable form; **callers branch on `code`, never on `error`**, which is
free to be reworded.

Four rules make the shape mean something:

1. **Nothing upstream is forwarded.** Not the status code, not the body, not
   the URL, not a header. A failing upstream is named only by its adapter id
   (`"source": "espocrm"`), which is enough to route the problem and discloses
   nothing about how to reach it.

2. **The kinds stay distinct.** `upstream_unavailable`, `permission_required`,
   `scope_required`, `not_found`, `invalid_cursor` and the module-specific
   codes are separate because they call for different actions: retry, ask for
   access, correct the request, stop.

3. **A denial says only what is missing.** It names the permission and nothing
   else — never whether the resource exists, never who owns it. A refusal that
   distinguishes "no such record" from "not yours" is an enumeration oracle.

4. **The enum is enforced.** A contract test reads the codes out of the
   handlers and fails on any the schema does not document, because a caller
   told to branch on `code` must be able to enumerate them from the contract.

## Consequences

Debugging a specific upstream failure requires the platform's logs rather than
the caller's response. That is the trade being made, and `request_id` is what
makes it workable: it is returned on every response, carried through adapters,
audit and events, and is what a user quotes when reporting a problem.

A new failure mode costs a code, a documented meaning and a test. That friction
is deliberate — it is what stops the error surface growing into a set of
strings nobody can switch on.
