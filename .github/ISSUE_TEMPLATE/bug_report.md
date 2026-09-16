---
name: Bug report
about: Something behaves differently from what the contract or documentation says
labels: bug
---

## What happened

## What should have happened

<!-- Where does the contract or documentation say so? A link makes this
reviewable instead of a matter of opinion. -->

## How to reproduce

<!-- The E2E stack needs no credentials and is the fastest reproduction for
anything platform-side:
  docker compose -f docker-compose.e2e.yml up -d --build --wait -->

## Correlation

<!-- The `X-Request-ID` from the failing response, if you have one. It traces
the request through the platform, its adapters and the audit trail, and turns
"it failed sometimes" into one specific request. -->

**Do not paste credentials, tokens or production data.** If reproducing needs
them, describe the shape instead.
