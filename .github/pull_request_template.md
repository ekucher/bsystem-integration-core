## What changed

<!-- One or two sentences. The diff shows what; this says what for. -->

## Why

<!-- What is this protecting against, or what does it make possible? A
reviewer cannot reconstruct the alternatives you rejected from the diff. -->

## How it was verified

<!-- Not "tests pass". Which command, against what. If a defect was found and
fixed while writing this, say so — that is the most useful line in the PR. -->

## Checklist

- [ ] Repository validation is green locally, not only in CI
- [ ] Tests cover the negative cases, not just the happy path
- [ ] Documentation updated where behaviour changed
- [ ] No check was weakened, skipped or allow-listed to get a green build
- [ ] No secret, credential or production payload is in the diff

<!-- Delete any that do not apply, and say why in a line. -->

- [ ] `docs/openapi.yaml` matches the routes served
- [ ] Migrations are additive, with no dropped column or table
- [ ] The E2E stack was run for changes that touch a database query
