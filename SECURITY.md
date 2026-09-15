# Security Policy

## Trust boundaries

Integration Core is a privileged platform component and must be treated as a security boundary.

## Mandatory controls

- Authenticate all service-to-service requests.
- Prefer OAuth2 Client Credentials / dedicated authentik service identities.
- Validate authorization and tenant/client scope before returning data.
- Never trust authorization claims supplied by an untrusted caller without verification.
- Never expose source-system credentials to HUB or browser clients.
- Store secrets outside Git.
- Validate webhook signatures/tokens where source systems support them.
- Rate-limit public or externally reachable endpoints.
- Log security-relevant operations with correlation IDs.
- Redact secrets from logs and AI context.

## AI-specific controls

AI access must be mediated through explicit policy. Integration Core must return only fields and entities the requesting identity is allowed to access.

## Data isolation

Cross-customer data access must be denied by backend checks. A client identifier in a URL or request body is never sufficient proof of access.

## Reporting

Do not publish exploitable security findings in public issues. Use the repository owner's private security process when configured.
