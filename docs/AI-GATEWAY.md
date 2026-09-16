# AI Gateway

`AI-GATEWAY-CONTRACT.md` states the architecture. This document describes what
is built and why it is built that way. `docs/openapi.yaml` is the authoritative
API contract.

## The sentence everything follows from

> AI is an authorized consumer of BSYSTEM data, not a bypass around
> authorization.

Three consequences, and each is a thing the gateway deliberately does not do.

### The gateway does not choose its own context

A request names Global IDs. There is no search, no similarity, no "related
records" and no "answer from what you know about us". A gateway that assembled
its own context would be deciding what a caller may see — and that decision
belongs to the authorization evaluator, which has a tenant model, scope grants
and a confinement rule that a retrieval heuristic does not.

### Each source is authorized as if read directly

Every accepted entity type is authorized with **exactly** the permission and
scope its own read endpoint uses:

| Source | Permission | Scope |
| --- | --- | --- |
| `client` | `crm.client.read` | client |
| `contact` | `crm.client.read` | resource |
| `project` | `projects.task.read` | project |
| `task` | `projects.task.read` | resource |
| `document` | `wiki.document.read` | resource |
| `server` | `operations.server.read` | resource, or the owning client |
| `incident` | `support.incident.read` | the client, or the record |

A test asserts this table against the route inventory. It is the property the
whole gateway rests on: a question must reach the same answer as the same
person reading the record directly. A gateway that asked for a weaker
permission would be the bypass the architecture forbids, and it would be
invisible — the endpoint would work, and only the wrong people would be able to
use it.

A source the caller may not use is **refused, not dropped**. Answering from a
smaller context would tell them the platform considered something it did not,
and the answer would read as though it accounted for the record they named. An
unresolvable source is refused identically, so the gateway cannot be used to
probe for identifiers that an endpoint would not disclose.

### The prompt carries the least that answers the question

Only normalized platform fields enter a prompt: a name, a status, a severity,
an environment. No upstream document body, no description, no incident summary
— those are the parts most likely to contain something nobody decided to send
to a model.

A prompt sent to a provider **has left the platform**. It may be logged by the
provider, retained, or used for training, and that is outside BSYSTEM's control
entirely. So the rule is not "redact what we must" but "send the least that
answers the question".

## Classification and redaction

`CREDENTIAL`-classified context is **refused**, not redacted. The caller asked
for something that cannot be answered safely, and quietly answering a narrower
question would hide that from them.

Everything that does go in is redacted, **including the caller's own
question** — a user pasting a token into a prompt is the likeliest way one
reaches a model, and the gateway is the last place to catch it.

The redactor handles three shapes:

1. **Keyed values** — `password: hunter2`, `api_key=…`, `Authorization: Bearer
   …`. The key is recognised and the value goes, whatever it looks like,
   because `password: 1` is still a password.
2. **Pasted secrets with no key** — "my token is sk-live-…". Recognised by the
   shape of the value: a known issuer prefix, or a long run of mixed letters
   and digits. A Global ID is too short, a word has no digits and a number has
   no letters, so ordinary prose survives.
3. **PEM blocks**, removed whole — including unterminated ones, which are still
   keys.

Matching is eager on purpose: a redacted field is an inconvenience, while a
credential in a prompt has already left the platform by the time anyone
notices. The opposite failure matters too, though — redaction that becomes
deletion leaves the model answering from nothing — so a test asserts ordinary
text passes through untouched.

**The test that matters** asserts against the payload the provider actually
received, not against the redactor in isolation. A redactor that works
perfectly and is called on the wrong string protects nothing, and that is
precisely the failure mode worth catching.

## Providers

| Provider | Configuration | Notes |
| --- | --- | --- |
| `fake` | none | the default |
| `ollama` | `OLLAMA_URL`, `OLLAMA_MODEL` | local; the prompt does not leave the deployment |
| `openai` | `OPENAI_URL`, `OPENAI_API_KEY`, `OPENAI_MODEL` | all three required, no defaults |

**The fake provider is the default, and that is not a placeholder.** It makes
the gateway — authorization, classification, redaction, audit, timeouts,
bounds — usable and testable in every deployment without a credential to hold
or a bill to pay. The parts of this package worth testing are the parts that
decide what leaves the platform, and those must be testable without anything
leaving it.

Credentials come from the environment and nowhere else. Nothing is committed,
and a misconfigured provider falls back to the fake with a warning rather than
silently sending prompts somewhere unintended. The cloud client refuses to
exist without an explicit endpoint, key and model: a default endpoint with a
missing key produces an authentication failure that reads like an outage.

A completion is not marked retryable. Retrying spends the same time again for a
different answer, which is not what idempotent means.

## Bounds

| Bound | Value |
| --- | --- |
| question | 4000 bytes |
| assembled prompt | 24000 bytes |
| sources per request | 20 |
| provider call | `AI_TIMEOUT`, default 30s |

These protect the platform — from cost, from latency, and from a caller
assembling a prompt large enough to be an exfiltration channel — not the
provider. The provider call is bounded independently of the caller's own
deadline, because a request left open is a request still costing money.

## Audit

Every call is recorded, answered or refused, and readable by administrators at
`GET /api/v1/ai/audit`.

**The prompt is not stored.** An audit trail is read by more people than the
request was, and storing the assembled context would recreate, in one
searchable table, exactly the aggregation the authorization rules exist to
prevent. What is recorded is who asked, which records they named, which
actually entered the prompt, what the platform sent it to, the classification
shape, the sizes and the duration — enough to answer the questions an audit is
for.

`bsystem_ai_requests_total` counts by provider and result. A rising
`refused_credential` count means somebody is repeatedly asking the platform to
send secrets to a model, which is worth seeing.

## Who may ask

`ai.query` is granted to **no role**. Who may spend money on a model, and whose
data may be put in front of one, is an owner decision about cost and exposure
rather than a technical default. Administrators reach it through the wildcard;
anyone else needs an explicit grant.
