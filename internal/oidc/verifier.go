package oidc

import (
	"context"
	"crypto"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Claims are what the platform reads out of a validated token.
//
// The names match authentik's, and the set is exactly what the authorization
// layer needs: who the principal is, and which groups decide their role.
// Nothing else is read, so a token growing new claims cannot change a
// decision by accident.
type Claims struct {
	Subject           string   `json:"sub"`
	Issuer            string   `json:"iss"`
	Email             string   `json:"email"`
	Name              string   `json:"name"`
	PreferredUsername string   `json:"preferred_username"`
	Groups            []string `json:"groups"`

	Audience  audience `json:"aud"`
	ExpiresAt int64    `json:"exp"`
	NotBefore int64    `json:"nbf"`
	IssuedAt  int64    `json:"iat"`
}

// audience is `aud`, which is a string or an array of strings depending on how
// many there are. A verifier that only handles one shape rejects perfectly
// valid tokens from a provider that sends the other.
type audience []string

func (a *audience) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*a = audience{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return errors.New("aud is neither a string nor an array of strings")
	}
	*a = many
	return nil
}

func (a audience) contains(value string) bool {
	for _, entry := range a {
		if entry == value {
			return true
		}
	}
	return false
}

// Errors a caller may distinguish. Everything else is a refusal with a reason
// in the message, which the platform logs and does not return.
var (
	// ErrNotConfigured means local validation was not set up, so the caller
	// should use whatever it did before.
	ErrNotConfigured = errors.New("no OIDC issuer is configured")
	// ErrInvalidToken means the token is not acceptable. It is one error on
	// purpose: telling a caller *why* their token failed tells an attacker
	// which of their guesses was closest.
	ErrInvalidToken = errors.New("invalid or expired token")
	// ErrProviderUnavailable means the platform could not obtain the keys it
	// needs. It is distinct because it is the platform's problem rather than
	// the caller's, and it deserves a different status code.
	ErrProviderUnavailable = errors.New("the identity provider's keys are unavailable")
)

// Config describes what a token must satisfy.
type Config struct {
	// Issuer is the exact `iss` a token must carry, and the base the
	// discovery document is fetched from.
	Issuer string
	// Audience is the client the token was minted for. Empty means the
	// audience is not checked, which is only defensible when a deployment has
	// exactly one client — it is logged at startup rather than assumed.
	Audience string
	// ClockSkew is how far the platform's clock may be behind or ahead of the
	// issuer's before a valid token is refused. Small: it is a tolerance for
	// drift, not an extension of a token's life.
	ClockSkew time.Duration
	// CacheTTL bounds how long a key set is used without being re-fetched.
	CacheTTL time.Duration
	// Timeout bounds every call to the provider. A discovery endpoint that
	// never answers must not hold a request open.
	Timeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.ClockSkew <= 0 {
		c.ClockSkew = 60 * time.Second
	}
	if c.CacheTTL <= 0 {
		c.CacheTTL = 15 * time.Minute
	}
	if c.Timeout <= 0 {
		c.Timeout = 5 * time.Second
	}
	return c
}

// Verifier validates tokens against a provider's published keys.
type Verifier struct {
	config Config
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	jwksURI   string
	keys      map[string]crypto.PublicKey
	fetchedAt time.Time
	// lastKidRefresh is when a fetch was last triggered *by an unknown kid*,
	// which is deliberately not the same as when the key set was last
	// fetched. A rotation has to be picked up on the first token that names
	// the new key; if an ordinary fetch counted here, a key set refreshed a
	// moment earlier would make the platform refuse every token from the new
	// key until the rate limit expired.
	lastKidRefresh time.Time
	refreshEvery   time.Duration
}

// New returns a verifier, or ErrNotConfigured when no issuer is set.
func New(config Config) (*Verifier, error) {
	config = config.withDefaults()
	if strings.TrimSpace(config.Issuer) == "" {
		return nil, ErrNotConfigured
	}
	return &Verifier{
		config: config,
		client: &http.Client{Timeout: config.Timeout},
		now:    time.Now,
		// A token naming a key the platform does not hold is the signal that
		// the provider rotated. Refreshing on it is what makes rotation work
		// without a restart — and rate-limiting it is what stops a stream of
		// forged kids turning the platform into a load generator pointed at
		// its own identity provider.
		refreshEvery: 30 * time.Second,
	}, nil
}

// Verify checks a raw bearer token and returns its claims.
func (v *Verifier) Verify(ctx context.Context, raw string) (Claims, error) {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 3 {
		// An opaque token is not a malformed JWT, but from here the two are
		// the same thing: this verifier cannot decide it.
		return Claims{}, fmt.Errorf("%w: not a signed token", ErrInvalidToken)
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: header is not base64url", ErrInvalidToken)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Claims{}, fmt.Errorf("%w: header is not JSON", ErrInvalidToken)
	}
	if header.Alg == "" || header.Alg == "none" {
		return Claims{}, fmt.Errorf("%w: unsigned token", ErrInvalidToken)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: signature is not base64url", ErrInvalidToken)
	}
	signed := []byte(parts[0] + "." + parts[1])

	key, err := v.keyFor(ctx, header.Kid)
	if err != nil {
		return Claims{}, err
	}
	if err := verifySignature(header.Alg, key, signed, signature); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: payload is not base64url", ErrInvalidToken)
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Claims{}, fmt.Errorf("%w: payload is not JSON", ErrInvalidToken)
	}
	if err := v.checkClaims(claims); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

// checkClaims is every check that does not involve cryptography.
//
// A valid signature says the issuer minted this token. It says nothing about
// whether the token was minted for this platform, or whether it is still
// alive, and a verifier that stops at the signature accepts a three-month-old
// token from a different client.
func (v *Verifier) checkClaims(claims Claims) error {
	if claims.Subject == "" {
		return fmt.Errorf("%w: no subject", ErrInvalidToken)
	}
	if claims.Issuer != v.config.Issuer {
		return fmt.Errorf("%w: issued by %q", ErrInvalidToken, claims.Issuer)
	}
	if v.config.Audience != "" && !claims.Audience.contains(v.config.Audience) {
		return fmt.Errorf("%w: not issued for this client", ErrInvalidToken)
	}
	now := v.now()
	if claims.ExpiresAt == 0 {
		// A token with no expiry never stops being valid, which makes a leaked
		// one permanent.
		return fmt.Errorf("%w: no expiry", ErrInvalidToken)
	}
	if now.After(time.Unix(claims.ExpiresAt, 0).Add(v.config.ClockSkew)) {
		return fmt.Errorf("%w: expired", ErrInvalidToken)
	}
	if claims.NotBefore != 0 && now.Before(time.Unix(claims.NotBefore, 0).Add(-v.config.ClockSkew)) {
		return fmt.Errorf("%w: not valid yet", ErrInvalidToken)
	}
	return nil
}

// keyFor returns the key a token names, refreshing the set when it names one
// the platform does not hold.
func (v *Verifier) keyFor(ctx context.Context, kid string) (crypto.PublicKey, error) {
	v.mu.Lock()
	fresh := v.keys != nil && v.now().Sub(v.fetchedAt) < v.config.CacheTTL
	if fresh {
		if key, ok := v.keys[kid]; ok {
			v.mu.Unlock()
			return key, nil
		}
		// An unknown kid on a fresh cache means a rotation, unless somebody is
		// making them up. The first one is believed and the rest are not,
		// until the window passes.
		if !v.lastKidRefresh.IsZero() && v.now().Sub(v.lastKidRefresh) < v.refreshEvery {
			v.mu.Unlock()
			return nil, fmt.Errorf("%w: unknown signing key", ErrInvalidToken)
		}
		v.lastKidRefresh = v.now()
	}
	v.mu.Unlock()

	keys, err := v.refresh(ctx)
	if err != nil {
		return nil, err
	}
	if key, ok := keys[kid]; ok {
		return key, nil
	}
	// A key set with exactly one key and a token with no kid is the common
	// single-key deployment.
	if kid == "" && len(keys) == 1 {
		for _, key := range keys {
			return key, nil
		}
	}
	return nil, fmt.Errorf("%w: unknown signing key", ErrInvalidToken)
}

// refresh fetches the key set, discovering the JWKS endpoint first if needed.
func (v *Verifier) refresh(ctx context.Context) (map[string]crypto.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, v.config.Timeout)
	defer cancel()

	v.mu.Lock()
	uri := v.jwksURI
	v.mu.Unlock()

	if uri == "" {
		discovered, err := v.discover(ctx)
		if err != nil {
			return nil, err
		}
		uri = discovered
	}

	body, err := v.get(ctx, uri)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	keys, err := parseJWKS(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}

	v.mu.Lock()
	v.jwksURI, v.keys, v.fetchedAt = uri, keys, v.now()
	v.mu.Unlock()
	return keys, nil
}

func (v *Verifier) discover(ctx context.Context) (string, error) {
	endpoint := strings.TrimRight(v.config.Issuer, "/") + "/.well-known/openid-configuration"
	body, err := v.get(ctx, endpoint)
	if err != nil {
		return "", fmt.Errorf("%w: discovery: %v", ErrProviderUnavailable, err)
	}
	var document struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return "", fmt.Errorf("%w: discovery document is not JSON", ErrProviderUnavailable)
	}
	// The document has to agree with the issuer it was fetched from. A
	// discovery document that names somebody else is either a
	// misconfiguration or a redirect somebody arranged, and following it would
	// mean trusting keys from whoever answered.
	if strings.TrimRight(document.Issuer, "/") != strings.TrimRight(v.config.Issuer, "/") {
		return "", fmt.Errorf("%w: discovery names issuer %q", ErrProviderUnavailable, document.Issuer)
	}
	if document.JWKSURI == "" {
		return "", fmt.Errorf("%w: discovery names no jwks_uri", ErrProviderUnavailable)
	}
	return document.JWKSURI, nil
}

func (v *Verifier) get(ctx context.Context, url string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := v.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	// Bounded: a key set is small, and a provider that answers with a stream
	// must not be able to exhaust the platform's memory.
	return io.ReadAll(io.LimitReader(response.Body, 1<<20))
}
