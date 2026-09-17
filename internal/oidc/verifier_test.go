package oidc

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A fake authentik.
//
// The point of a verifier is what it refuses, and a real provider will not
// issue a token signed with the wrong algorithm, addressed to somebody else,
// or expired last week. So the tests mint their own.

type provider struct {
	server   *httptest.Server
	keys     map[string]*rsa.PrivateKey
	issuer   string
	jwksHits int64
	// down makes every request to the provider fail, which is how an outage
	// is reproduced without stopping the server and losing its address.
	down atomic.Bool
	// hideDiscovery answers 404 on the discovery document only.
	hideDiscovery atomic.Bool
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	p := &provider{keys: map[string]*rsa.PrivateKey{"key-1": key}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if p.down.Load() || p.hideDiscovery.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   p.issuer,
			"jwks_uri": p.issuer + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&p.jwksHits, 1)
		if p.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		entries := []map[string]string{}
		for kid, key := range p.keys {
			entries = append(entries, map[string]string{
				"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": entries})
	})
	p.server = httptest.NewServer(mux)
	p.issuer = p.server.URL
	t.Cleanup(p.server.Close)
	return p
}

func (p *provider) rotate(t *testing.T, kid string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	p.keys[kid] = key
}

// mint signs a token with the named key, applying whatever the test wants
// changed about it.
func (p *provider) mint(t *testing.T, kid string, edit func(header map[string]any, claims map[string]any)) string {
	t.Helper()
	key, ok := p.keys[kid]
	if !ok {
		t.Fatalf("no such key %q", kid)
	}
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	claims := map[string]any{
		"sub": "ak-user-1", "iss": p.issuer, "aud": "bsystem-hub",
		"email": "person@example.invalid", "name": "A Person",
		"preferred_username": "person", "groups": []string{"BSYSTEM-Admins"},
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Add(-time.Minute).Unix(),
	}
	if edit != nil {
		edit(header, claims)
	}
	encodedHeader := encodeSegment(t, header)
	encodedClaims := encodeSegment(t, claims)
	signed := encodedHeader + "." + encodedClaims

	alg, _ := header["alg"].(string)
	switch alg {
	case "none":
		return signed + "."
	case "HS256":
		// The classic confusion: the attacker has the provider's *public* key,
		// because it is published, and signs the token with it as if it were a
		// shared secret. A verifier that picks its algorithm from the header
		// and its key from the key set accepts this.
		public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatalf("marshal public key: %v", err)
		}
		mac := hmac.New(sha256.New, public)
		mac.Write([]byte(signed))
		return signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	default:
		digest := sha256.Sum256([]byte(signed))
		signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
	}
}

func encodeSegment(t *testing.T, value map[string]any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode segment: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(body)
}

func verifierFor(t *testing.T, p *provider) *Verifier {
	t.Helper()
	verifier, err := New(Config{Issuer: p.issuer, Audience: "bsystem-hub", CacheTTL: time.Minute})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	return verifier
}

func TestAValidTokenIsAcceptedAndItsClaimsAreRead(t *testing.T) {
	p := newProvider(t)
	verifier := verifierFor(t, p)

	claims, err := verifier.Verify(context.Background(), p.mint(t, "key-1", nil))
	if err != nil {
		t.Fatalf("a valid token was refused: %v", err)
	}
	if claims.Subject != "ak-user-1" {
		t.Errorf("subject = %q", claims.Subject)
	}
	if claims.PreferredUsername != "person" || claims.Email != "person@example.invalid" {
		t.Errorf("identity claims not read: %+v", claims)
	}
	if len(claims.Groups) != 1 || claims.Groups[0] != "BSYSTEM-Admins" {
		t.Errorf("groups = %v; the authorization layer reads these", claims.Groups)
	}
	// Local validation means exactly one call to the provider, for its keys,
	// and none per request afterwards. That is the whole point of the change.
	before := atomic.LoadInt64(&p.jwksHits)
	for i := 0; i < 20; i++ {
		if _, err := verifier.Verify(context.Background(), p.mint(t, "key-1", nil)); err != nil {
			t.Fatalf("repeat verification: %v", err)
		}
	}
	if after := atomic.LoadInt64(&p.jwksHits); after != before {
		t.Errorf("20 verifications made %d further calls to the provider; validity is supposed to be decided locally", after-before)
	}
}

// Everything a token can be wrong about, in one table. Each row is a token a
// careless verifier accepts.
func TestTheTokensAVerifierMustRefuse(t *testing.T) {
	p := newProvider(t)
	verifier := verifierFor(t, p)
	// Warm the cache so a refusal is about the token rather than about the
	// provider being consulted.
	if _, err := verifier.Verify(context.Background(), p.mint(t, "key-1", nil)); err != nil {
		t.Fatalf("warm: %v", err)
	}

	tests := []struct {
		name  string
		token func() string
	}{
		{"expired", func() string {
			return p.mint(t, "key-1", func(_, claims map[string]any) {
				claims["exp"] = time.Now().Add(-2 * time.Hour).Unix()
			})
		}},
		{"no expiry at all", func() string {
			return p.mint(t, "key-1", func(_, claims map[string]any) { delete(claims, "exp") })
		}},
		{"not valid yet", func() string {
			return p.mint(t, "key-1", func(_, claims map[string]any) {
				claims["nbf"] = time.Now().Add(time.Hour).Unix()
			})
		}},
		{"issued by somebody else", func() string {
			return p.mint(t, "key-1", func(_, claims map[string]any) {
				claims["iss"] = "https://impostor.example"
			})
		}},
		{"issued for another client", func() string {
			return p.mint(t, "key-1", func(_, claims map[string]any) { claims["aud"] = "some-other-app" })
		}},
		{"no subject", func() string {
			return p.mint(t, "key-1", func(_, claims map[string]any) { delete(claims, "sub") })
		}},
		{"unsigned", func() string {
			return p.mint(t, "key-1", func(header, _ map[string]any) { header["alg"] = "none" })
		}},
		{"signed with the provider's public key as an HMAC secret", func() string {
			return p.mint(t, "key-1", func(header, _ map[string]any) { header["alg"] = "HS256" })
		}},
		{"signed by a key the provider never published", func() string {
			other := newProvider(t)
			return other.mint(t, "key-1", func(_, claims map[string]any) { claims["iss"] = p.issuer })
		}},
		{"payload tampered with after signing", func() string {
			token := p.mint(t, "key-1", nil)
			parts := strings.Split(token, ".")
			forged := encodeSegment(t, map[string]any{
				"sub": "ak-user-1", "iss": p.issuer, "aud": "bsystem-hub",
				"groups": []string{"BSYSTEM-Admins"}, "exp": time.Now().Add(time.Hour).Unix(),
			})
			return parts[0] + "." + forged + "." + parts[2]
		}},
		{"not a token at all", func() string { return "test-token-admin" }},
		{"three parts of nonsense", func() string { return "a.b.c" }},
		{"empty", func() string { return "" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims, err := verifier.Verify(context.Background(), test.token())
			if err == nil {
				t.Fatalf("accepted, with subject %q", claims.Subject)
			}
			if !errors.Is(err, ErrInvalidToken) {
				t.Errorf("refused for the wrong reason: %v", err)
			}
		})
	}
}

// Key rotation is the case that decides whether the cache is an optimisation
// or an outage waiting for a maintenance window.
func TestARotatedSigningKeyWorksWithoutARestart(t *testing.T) {
	p := newProvider(t)
	verifier := verifierFor(t, p)
	if _, err := verifier.Verify(context.Background(), p.mint(t, "key-1", nil)); err != nil {
		t.Fatalf("before rotation: %v", err)
	}

	// The provider adds a key and starts signing with it. The platform has
	// never seen this kid and its cache is still fresh.
	p.rotate(t, "key-2")
	if _, err := verifier.Verify(context.Background(), p.mint(t, "key-2", nil)); err != nil {
		t.Fatalf("a token signed by a newly published key was refused: %v", err)
	}
	// And the old key still works while the provider still publishes it,
	// which is what makes a rotation a rotation rather than a cutover.
	if _, err := verifier.Verify(context.Background(), p.mint(t, "key-1", nil)); err != nil {
		t.Errorf("a token signed by the previous key was refused during rotation: %v", err)
	}
}

// Refreshing on an unknown kid is what makes rotation work. Doing it on every
// unknown kid is what turns a stream of forged tokens into a load generator
// pointed at the platform's own identity provider.
func TestForgedKeyIdentifiersDoNotStampedeTheProvider(t *testing.T) {
	p := newProvider(t)
	verifier := verifierFor(t, p)
	if _, err := verifier.Verify(context.Background(), p.mint(t, "key-1", nil)); err != nil {
		t.Fatalf("warm: %v", err)
	}
	before := atomic.LoadInt64(&p.jwksHits)

	for i := 0; i < 50; i++ {
		token := p.mint(t, "key-1", func(header, _ map[string]any) {
			header["kid"] = fmt.Sprintf("invented-%d", i)
		})
		if _, err := verifier.Verify(context.Background(), token); err == nil {
			t.Fatal("a token naming a key nobody published was accepted")
		}
	}
	if fetches := atomic.LoadInt64(&p.jwksHits) - before; fetches > 2 {
		t.Errorf("50 forged key ids caused %d key-set fetches; the refresh is not rate limited", fetches)
	}
}

// An identity provider that is unreachable must not be able to make an invalid
// token valid, and must not be able to stop a valid one from working while the
// platform still holds usable keys.
func TestAProviderOutageNeitherGrantsNorRevokes(t *testing.T) {
	p := newProvider(t)
	verifier := verifierFor(t, p)
	valid := p.mint(t, "key-1", nil)
	if _, err := verifier.Verify(context.Background(), valid); err != nil {
		t.Fatalf("warm: %v", err)
	}

	p.down.Store(true)

	// Still good: the keys are held, the token is signed, nothing about its
	// validity depends on the provider answering right now.
	if _, err := verifier.Verify(context.Background(), p.mint(t, "key-1", nil)); err != nil {
		t.Errorf("a valid token was refused because the provider was unreachable: %v", err)
	}
	// Still bad: an outage is not an excuse to skip a check.
	expired := p.mint(t, "key-1", func(_, claims map[string]any) {
		claims["exp"] = time.Now().Add(-time.Hour).Unix()
	})
	if _, err := verifier.Verify(context.Background(), expired); err == nil {
		t.Error("an expired token was accepted while the provider was unreachable")
	}
}

// A platform that has never reached its provider holds no keys, and must
// refuse rather than fall back to something weaker. The refusal is the
// platform's problem rather than the caller's, and says so.
func TestAProviderThatWasNeverReachableRefusesEverything(t *testing.T) {
	p := newProvider(t)
	token := p.mint(t, "key-1", nil)
	p.down.Store(true)

	verifier := verifierFor(t, p)
	_, err := verifier.Verify(context.Background(), token)
	if err == nil {
		t.Fatal("a token was accepted by a verifier that had never fetched a key")
	}
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("error = %v, want one identifying the provider as the problem: a caller told their token is invalid will go and get another one", err)
	}
}

// A discovery document that names a different issuer is either a
// misconfiguration or a redirect somebody arranged. Following it would mean
// taking keys from whoever answered.
func TestADiscoveryDocumentNamingAnotherIssuerIsRefused(t *testing.T) {
	p := newProvider(t)
	verifier, err := New(Config{Issuer: p.issuer, Audience: "bsystem-hub"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// The verifier is configured for this issuer; the document will name it
	// correctly, so flip the verifier's expectation instead.
	verifier.config.Issuer = p.issuer + "/elsewhere"
	_, err = verifier.Verify(context.Background(), p.mint(t, "key-1", nil))
	if !errors.Is(err, ErrProviderUnavailable) && !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if errors.Is(err, ErrInvalidToken) && !strings.Contains(err.Error(), "issued by") {
		t.Errorf("the refusal is not about the issuer mismatch: %v", err)
	}
}

// Clocks drift. The tolerance is a tolerance, not an extension: a token one
// second inside it is fine and one far outside it is not.
func TestClockSkewIsToleratedButNotUnbounded(t *testing.T) {
	p := newProvider(t)
	verifier, err := New(Config{Issuer: p.issuer, Audience: "bsystem-hub", ClockSkew: 30 * time.Second})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	justExpired := p.mint(t, "key-1", func(_, claims map[string]any) {
		claims["exp"] = time.Now().Add(-10 * time.Second).Unix()
	})
	if _, err := verifier.Verify(context.Background(), justExpired); err != nil {
		t.Errorf("a token ten seconds past expiry was refused with a thirty-second tolerance: %v", err)
	}
	wellExpired := p.mint(t, "key-1", func(_, claims map[string]any) {
		claims["exp"] = time.Now().Add(-5 * time.Minute).Unix()
	})
	if _, err := verifier.Verify(context.Background(), wellExpired); err == nil {
		t.Error("a token five minutes past expiry was accepted; the tolerance has become an extension")
	}
}

// `aud` is a string or an array depending on how many there are. A verifier
// that handles one shape refuses perfectly valid tokens from a provider that
// sends the other.
func TestAnAudienceArrayIsAccepted(t *testing.T) {
	p := newProvider(t)
	verifier := verifierFor(t, p)
	token := p.mint(t, "key-1", func(_, claims map[string]any) {
		claims["aud"] = []string{"another-app", "bsystem-hub"}
	})
	if _, err := verifier.Verify(context.Background(), token); err != nil {
		t.Errorf("a token listing this platform among several audiences was refused: %v", err)
	}
	wrong := p.mint(t, "key-1", func(_, claims map[string]any) {
		claims["aud"] = []string{"another-app", "a-third-app"}
	})
	if _, err := verifier.Verify(context.Background(), wrong); err == nil {
		t.Error("a token naming several audiences, none of them this platform, was accepted")
	}
}

// A key set is not a place to accept whatever arrives. A short RSA modulus is
// a key somebody can factor, and a verifier that loads it will believe
// anything signed with it.
func TestAWeakOrUnusableKeyIsNotLoaded(t *testing.T) {
	short, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	body := fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":"weak","use":"sig","n":%q,"e":%q}]}`,
		base64.RawURLEncoding.EncodeToString(short.N.Bytes()),
		base64.RawURLEncoding.EncodeToString(big.NewInt(int64(short.E)).Bytes()))
	if _, err := parseJWKS([]byte(body)); err == nil {
		t.Error("a 1024-bit RSA key was loaded as a signing key")
	}

	// And a set that is only an encryption key is not a signing key set.
	encryption := `{"keys":[{"kty":"RSA","kid":"enc","use":"enc","n":"AQAB","e":"AQAB"}]}`
	if _, err := parseJWKS([]byte(encryption)); err == nil {
		t.Error("a key set with no signing key was accepted")
	}
}

func TestNoIssuerMeansNotConfigured(t *testing.T) {
	if _, err := New(Config{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("error = %v, want ErrNotConfigured", err)
	}
	if _, err := New(Config{Issuer: "   "}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("whitespace issuer = %v, want ErrNotConfigured", err)
	}
}

// The discovery document decides what the platform fetches next. Without a
// constraint on it, a provider that has been misconfigured — or an answer from
// something else on the network — can point the Core at any address it can
// reach, and in the worst case the platform loads signing keys from there.
//
// Every OIDC provider publishes its key set under the issuer's own host, so
// the constraint costs a correct deployment nothing.
func TestAKeySetSomewhereOtherThanTheIssuerIsRefused(t *testing.T) {
	elsewhere := newProvider(t)

	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// The issuer is its own, so the check that the document names the
		// issuer it was fetched from passes. The key set is not.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   "http://" + r.Host,
			"jwks_uri": elsewhere.issuer + "/jwks",
		})
	}))
	t.Cleanup(redirecting.Close)

	verifier, err := New(Config{Issuer: redirecting.URL, Audience: "bsystem-hub"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// A token signed by the other provider's key, which is exactly what the
	// redirection would make acceptable.
	token := elsewhere.mint(t, "key-1", func(_, claims map[string]any) {
		claims["iss"] = redirecting.URL
	})
	_, err = verifier.Verify(context.Background(), token)
	if err == nil {
		t.Fatal("a token was accepted using keys fetched from an address the discovery document chose")
	}
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("error = %v, want one identifying the provider's configuration as the problem", err)
	}
}
