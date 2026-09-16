# ADR-010: AI routing policy

Status: Accepted

Date: 2026-09-16

## Context

`AI-GATEWAY-CONTRACT.md` states the principle: AI is an authorized consumer of
BSYSTEM data, not a bypass around authorization. This ADR records how provider
selection and context assembly implement it, because those are where the
principle is either kept or quietly lost.

A prompt sent to a provider **has left the platform**. It may be logged by the
provider, retained, or used for training, and none of that is under BSYSTEM's
control or visible in its audit trail. Every decision below follows from
treating that as irreversible.

## Decision

### Provider selection is configuration, never a prompt

`AI_PROVIDER` chooses. A caller cannot select a provider, and no text in a
question can change where the request goes. Provider choice is a policy about
where data may travel; letting a prompt influence it would make that policy
settable by whoever is asking.

| Provider | Configuration | Note |
| --- | --- | --- |
| `fake` | none | the default |
| `ollama` | `OLLAMA_URL`, `OLLAMA_MODEL` | local: the prompt does not leave the deployment |
| `openai` | `OPENAI_URL`, `OPENAI_API_KEY`, `OPENAI_MODEL` | all three required |

**The fake provider is the default, and that is not a placeholder.** It makes
authorization, classification, redaction, audit and the bounds exercisable in
every deployment without a credential to hold or a bill to pay. The parts of
this system worth testing are the parts that decide what leaves the platform,
and those must be testable without anything leaving it.

A misconfigured provider **falls back to the fake with a warning**, rather than
failing open to a partially configured one. The cloud client refuses to exist
without an explicit endpoint, key and model: a default endpoint with a missing
key produces an authentication failure that reads like an outage.

Credentials come from the environment and nowhere else.

### The caller names the context; the gateway never widens it

A request carries Global IDs. There is no search, no similarity and no "related
records". A gateway that assembled its own context would be deciding what a
caller may see — and that decision belongs to the evaluator, which has a tenant
model, scope grants and a confinement rule that a retrieval heuristic does not.

Each named source is authorized with **exactly** the permission and scope its
own read endpoint uses, asserted by a test against the route inventory. A
weaker permission here would be the forbidden bypass, and it would be
invisible: the endpoint would work, and only the wrong people could use it.

A source the caller may not use is **refused, not dropped**. Answering from a
smaller context would tell them the platform considered something it did not.
An unresolvable source is refused identically, so the gateway cannot be used to
probe for identifiers.

### Only the least that answers the question goes in

Normalized platform fields only — a name, a status, a severity. No upstream
document body, no description, no incident summary: those are the parts most
likely to contain something nobody decided to send to a model.

`CREDENTIAL`-classified context is **refused rather than redacted**: the caller
asked for something that cannot be answered safely, and quietly answering a
narrower question would hide that. Everything else is redacted, **including the
caller's own question**, because a user pasting a token into a prompt is the
likeliest way one reaches a model.

Redaction is eager on purpose. A redacted field is an inconvenience; a
credential in a prompt has already left the platform by the time anyone
notices. The opposite failure is real too — redaction that becomes deletion
leaves the model answering from nothing — so ordinary text is tested to pass
through untouched.

### The audit records the shape, not the content

Who asked, which records they named, which entered the prompt, which provider
and model, the classification summary, the sizes, the outcome.

**No prompt and no answer.** An audit trail is read by more people than the
request was, and storing the assembled context would recreate — in one
searchable table with one permission in front of it — exactly the aggregation
the authorization rules exist to prevent.

### Nobody may ask by default

`ai.query` is granted to no role. Who may spend money on a model, and whose
data may be put in front of one, is an owner decision about cost and exposure.
Administrators reach it through the wildcard; anyone else needs an explicit
grant.

## Consequences

The gateway cannot answer open questions about the platform. "Which clients are
at risk?" is not answerable; "summarise these three incidents" is. That is the
intended shape: the caller has already been authorized for what they named.

A deployment that wants above-`INTERNAL` data in prompts should route to the
local provider, where the prompt does not leave. Nothing in the code enforces
that today — it is a configuration decision — and making the classification
gate provider choice is the obvious next step once more than one provider is
actually deployed.
