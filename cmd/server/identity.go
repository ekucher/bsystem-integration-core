package main

// Resolving a bearer token into an identity.
//
// There are two ways to do it and the platform supports both, because they
// suit different deployments and the wrong one cannot be chosen by accident.
//
// Locally, from the token's own signature, when an issuer is configured. The
// identity provider is consulted once for its keys and then not again, so an
// authentik that is slow does not make every read slow and an authentik that
// is down does not make every token look invalid.
//
// Remotely, by asking authentik's UserInfo endpoint, when it is not. That is
// what the platform has always done, and it stays the default: it is the only
// thing that works with an opaque access token, and a deployment whose
// provider issues those has not been broken by this change.
//
// What must not happen is a silent downgrade between them. A platform
// configured for local validation that quietly asked UserInfo whenever a token
// did not parse would have the network dependency it was configured to remove,
// reachable by anyone who sends a token that does not parse.

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/oidc"
)

// tokenVerifier builds the local verifier from the environment, or returns nil
// when no issuer is configured.
func tokenVerifier() *oidc.Verifier {
	issuer := strings.TrimSpace(os.Getenv("OIDC_ISSUER_URL"))
	if issuer == "" {
		return nil
	}
	audience := strings.TrimSpace(os.Getenv("OIDC_AUDIENCE"))
	if audience == "" {
		// Not fatal, and not silent. A deployment with one client is the
		// normal case and checking the audience then adds nothing; a
		// deployment with several and no audience configured accepts tokens
		// minted for any of them.
		logger.Warn("OIDC_AUDIENCE is not set: a token minted for any client of this issuer will be accepted",
			"issuer", issuer)
	}
	verifier, err := oidc.New(oidc.Config{
		Issuer:    issuer,
		Audience:  audience,
		ClockSkew: durationEnv("OIDC_CLOCK_SKEW", 60*time.Second),
		CacheTTL:  durationEnv("OIDC_JWKS_TTL", 15*time.Minute),
		Timeout:   durationEnv("OIDC_HTTP_TIMEOUT", 5*time.Second),
	})
	if err != nil {
		logger.Error("local token validation could not be configured", "error", err.Error())
		return nil
	}
	logger.Info("validating tokens locally", "issuer", issuer, "audience_checked", audience != "")
	return verifier
}

// identify resolves a bearer token into the identity the platform authorizes.
func (a *app) identify(ctx context.Context, token string) (userInfo, error) {
	if a.tokens == nil {
		return fetchUserInfo(ctx, token)
	}
	claims, err := a.tokens.Verify(ctx, token)
	if err != nil {
		return userInfo{}, err
	}
	info := userInfo{
		Sub:               claims.Subject,
		Email:             claims.Email,
		Name:              claims.Name,
		PreferredUsername: claims.PreferredUsername,
		Groups:            claims.Groups,
	}
	if len(info.Groups) == 0 {
		info = a.enrich(ctx, token, info)
	}
	return info, nil
}

// enrich fills in what the token did not carry, from UserInfo.
//
// Some authentik configurations leave `groups` out of the access token and
// serve it from UserInfo only. That deployment still works, and the enrichment
// is bounded by two rules that keep the outage story intact:
//
//   - a failure is not an error. The request proceeds with what the token
//     said, which is no groups, which is no role — deny-by-default, not a
//     broader one. An unreachable UserInfo can cost a caller their access; it
//     can never give them somebody else's.
//   - an answer about a different subject is discarded. UserInfo is called
//     with the caller's own token so this should not happen, and if it does,
//     the thing being described is not the principal the signature identified.
func (a *app) enrich(ctx context.Context, token string, info userInfo) userInfo {
	if strings.TrimSpace(os.Getenv("AUTHENTIK_USERINFO_URL")) == "" {
		return info
	}
	enriched, err := fetchUserInfo(ctx, token)
	if err != nil {
		logger.WarnContext(ctx, "the token carried no groups and UserInfo could not be reached; continuing with no groups",
			"error", err.Error())
		return info
	}
	if enriched.Sub != info.Sub {
		logger.ErrorContext(ctx, "UserInfo described a different subject than the token; the enrichment was discarded")
		return info
	}
	info.Groups = enriched.Groups
	if info.Email == "" {
		info.Email = enriched.Email
	}
	if info.Name == "" {
		info.Name = enriched.Name
	}
	if info.PreferredUsername == "" {
		info.PreferredUsername = enriched.PreferredUsername
	}
	return info
}

// authenticationStatus maps a resolution failure onto a status code.
//
// The distinction matters to the caller: a 401 means go and get another token,
// and a 503 means the platform cannot check the one you have. Answering 401
// for the second sends every signed-in person to the login page during an
// outage that has nothing to do with their session.
func authenticationStatus(err error) (int, string) {
	if errors.Is(err, oidc.ErrProviderUnavailable) {
		return 503, "identity provider unavailable"
	}
	return 401, "invalid or expired token"
}
