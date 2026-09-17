# Authentication

authentik is the issuer and the source of identity. This document is about how
the platform decides a token is genuine, which is a different question.

## Two modes

| | Local validation | UserInfo |
| --- | --- | --- |
| Enabled by | `OIDC_ISSUER_URL` | the absence of it (the default) |
| Decides validity from | the token's signature | authentik's answer |
| Calls to authentik | once for its keys, then not per request | **one per request** |
| Works with an opaque access token | no | yes |
| An authentik outage means | signed-in callers keep working | every token looks invalid |

Local validation is the better mode and it is not the default, because
switching a deployment to it requires authentik to be issuing JWT access
tokens. A deployment whose provider issues opaque tokens is not broken by this
being available; it simply does not set the variable.

### There is no downgrade between them

A platform configured to validate locally refuses a token it cannot verify. It
does **not** fall back to asking UserInfo, because that would restore the
network dependency the configuration exists to remove, reachable by anybody who
sends something that is not a JWT.

## Configuration

| Variable | Required | Default | Notes |
| --- | --- | --- | --- |
| `OIDC_ISSUER_URL` | to enable local validation | — | The exact `iss` a token must carry. Discovery is fetched from it. |
| `OIDC_AUDIENCE` | strongly recommended | — | The client the token was minted for. Empty accepts a token minted for **any** client of this issuer, and is logged as a warning at startup. |
| `OIDC_CLOCK_SKEW` | no | `60s` | Tolerance for drift between the platform's clock and the issuer's. A tolerance, not an extension. |
| `OIDC_JWKS_TTL` | no | `15m` | How long a key set is used before it is re-fetched. |
| `OIDC_HTTP_TIMEOUT` | no | `5s` | Bounds every call to the provider. |
| `AUTHENTIK_USERINFO_URL` | for the default mode | — | Also used for group enrichment when local validation is on and the token carries no `groups`. |

## Claims required

```json
{
  "iss": "https://authentik.example/application/o/bsystem-hub/",
  "sub": "ak-user-9f2c",
  "aud": "bsystem-hub",
  "exp": 1767225600,
  "preferred_username": "person",
  "email": "person@example.invalid",
  "name": "A Person",
  "groups": ["BSYSTEM-Admins"]
}
```

- **`sub`** is the identity. It is what a `USR-*` is allocated against and it
  must be stable for the life of the account: a subject that changes mints a
  second Global ID for the same person.
- **`exp`** is required. A token with no expiry never stops being valid, which
  makes a leaked one permanent.
- **`groups`** decides the role. See `AUTHORIZATION.md` for the mapping.
- `iat` and `nbf` are honoured when present.

In authentik this means the provider's scope mapping must put `groups` (and the
profile claims) into the **access token**, not only into UserInfo. Where that
is not configured, the platform falls back to UserInfo for the groups alone —
see below.

## What is refused

A signature that verifies says the issuer minted the token. It says nothing
about whether the token was minted for this platform or whether it is still
alive, so all of these are refused:

| Refused | Why it matters |
| --- | --- |
| `alg: none` | an unsigned token is an assertion by its bearer |
| `HS256` and every other HMAC | the provider's public key is published; a verifier that treats it as a shared secret accepts a token anybody can mint |
| a key the provider never published | including a correctly formed token from a different provider |
| an unknown `kid` not explained by a rotation | see below |
| an RSA key under 2048 bits | a key somebody can factor is a key that signs anything |
| a wrong `iss` or `aud` | a valid token from elsewhere is still not a token for here |
| expired, or not yet valid | beyond the configured tolerance |
| no `sub`, no `exp` | there is nothing to authorize and nothing to expire |
| a tampered payload | the signature covers header and payload |
| an opaque token | when local validation is configured — no downgrade |

A refusal is always the same message to the caller. Telling somebody *which*
check their token failed tells an attacker which of their guesses was closest.
The reason is logged; **the token never is**, in whole or in part.

### The discovery document is not trusted to redirect

Two things are checked about it, because it decides what the platform fetches
next:

- it must **name the issuer it was fetched from**. A document naming somebody
  else is a misconfiguration or a redirect somebody arranged, and following it
  means taking keys from whoever answered.
- its `jwks_uri` must be on the **issuer's own origin**. Without that, the
  document chooses any address the Core can reach, and the platform makes an
  outbound request somewhere it was never told about — in the worst case
  loading signing keys from it. Every OIDC provider publishes its key set under
  the issuer's host, so a correct deployment pays nothing for this.

## Key rotation

A token naming a `kid` the platform does not hold triggers one refresh of the
key set, so a rotation is picked up on the first token signed with the new key
and needs no restart. The previous key keeps working for as long as the
provider publishes it, which is what makes a rotation a rotation rather than a
cutover.

That refresh is rate-limited to once every 30 seconds. Without the limit, a
stream of tokens carrying invented `kid`s turns the platform into a load
generator pointed at its own identity provider.

## Group enrichment, and what it cannot do

When local validation is on and a token carries no `groups`, the platform calls
UserInfo once for that request. Two rules bound it:

- **A failure is not an error.** The request proceeds with what the token said,
  which is no groups, which is no role. Authorization is deny-by-default, so an
  unreachable UserInfo can cost a caller their access — it can never give them
  somebody else's.
- **An answer about a different subject is discarded.** UserInfo is called with
  the caller's own token, so it should not happen; if it does, what is being
  described is not the principal the signature identified.

A token that carries `groups` never triggers the call at all, which is the
configuration to aim for.

## Status codes

`401` means the token is not acceptable — go and get another one. `503` means
the platform could not obtain the keys it needs to check the one you have.

The distinction is not cosmetic: answering `401` for a provider outage sends
every signed-in person to the login page during an incident that has nothing to
do with their session, where they will fail to sign in as well.
