package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/oidc"
)

// A signing identity provider, for the middleware rather than for the
// verifier: internal/oidc proves what a token must satisfy, and this proves
// that the platform asks it, believes the answer, and behaves correctly when
// the provider is unreachable.

type signingProvider struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	issuer string
	down   atomic.Bool
}

func newSigningProvider(t *testing.T) *signingProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	p := &signingProvider{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if p.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": p.issuer, "jwks_uri": p.issuer + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		if p.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "key-1",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	p.server = httptest.NewServer(mux)
	p.issuer = p.server.URL
	t.Cleanup(p.server.Close)
	return p
}

func (p *signingProvider) token(t *testing.T, subject string, groups []string) string {
	t.Helper()
	claims := map[string]any{
		"sub": subject, "iss": p.issuer, "aud": "bsystem-hub",
		"email": subject + "@example.invalid", "preferred_username": subject,
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	if groups != nil {
		claims["groups"] = groups
	}
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "key-1"})
	payload, _ := json.Marshal(claims)
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// locallyValidatingApp is the fixture app with local validation switched on.
func locallyValidatingApp(t *testing.T, p *signingProvider, principals map[string]map[string]any) (*app, http.Handler) {
	t.Helper()
	application, _ := integrationApp(t, principals)
	verifier, err := oidc.New(oidc.Config{Issuer: p.issuer, Audience: "bsystem-hub", CacheTTL: time.Minute})
	if err != nil {
		t.Fatalf("configure local validation: %v", err)
	}
	application.tokens = verifier
	return application, application.handler()
}

// The change this makes: a request authenticates from the token's own
// signature, and the identity provider is not in the path.
func TestASignedTokenAuthenticatesWithoutAskingTheProvider(t *testing.T) {
	provider := newSigningProvider(t)
	_, handler := locallyValidatingApp(t, provider, nil)

	token := provider.token(t, "ak-admin", []string{"BSYSTEM-Admins"})
	recorder := call(t, handler, http.MethodGet, "/api/v1/me", token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
	var me struct {
		Subject string   `json:"subject"`
		Roles   []string `json:"roles"`
		ID      string   `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if me.Subject != "ak-admin" {
		t.Errorf("subject = %q, want the token's", me.Subject)
	}
	if !strings.HasPrefix(me.ID, "USR-") {
		t.Errorf("id = %q, want a USR-* Global ID: a locally validated principal is still a platform identity", me.ID)
	}
	if len(me.Roles) == 0 {
		t.Errorf("the token's groups did not resolve to a role: %+v", me)
	}

	// The provider goes away entirely. Everything above still works, which is
	// the whole reason for the change: authentik is the issuer, not a
	// synchronous dependency of every read.
	provider.down.Store(true)
	// A route with no upstream behind it, so the only thing that can fail is
	// authentication.
	after := call(t, handler, http.MethodGet, "/api/v1/me", token)
	if after.Code != http.StatusOK {
		t.Errorf("a request with a valid token answered %d while the identity provider was down (body: %s)", after.Code, after.Body.String())
	}
}

// The guarantee that matters most about the enrichment path: an unreachable
// UserInfo can cost a caller their access. It can never give them somebody
// else's, and it can never make an invalid token valid.
func TestAUserInfoOutageNarrowsAccessAndNeverWidensIt(t *testing.T) {
	provider := newSigningProvider(t)
	_, handler := locallyValidatingApp(t, provider, map[string]map[string]any{})

	// A token with no groups claim: the deployment where authentik serves
	// groups from UserInfo only. The identity fixture knows no such subject,
	// so the enrichment call fails exactly as an outage would.
	tokenWithoutGroups := provider.token(t, "ak-ungrouped", nil)
	recorder := call(t, handler, http.MethodGet, "/api/v1/clients", tokenWithoutGroups)
	if recorder.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403: a principal whose groups could not be resolved must get no role, not a default one (body: %s)", recorder.Code, recorder.Body.String())
	}
	// Still authenticated, though — the signature said who they are. The
	// profile endpoint needs no permission and must answer, and what it says
	// about their roles is the direct form of the claim above: no groups
	// resolved means no role, not a default one.
	profile := call(t, handler, http.MethodGet, "/api/v1/me", tokenWithoutGroups)
	if profile.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/me = %d, want 200: the token itself was valid", profile.Code)
	}
	var me struct {
		Roles       []string `json:"roles"`
		Groups      []string `json:"groups"`
		Permissions []string `json:"permissions"`
	}
	if err := json.Unmarshal(profile.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode profile: %v", err)
	}
	if len(me.Groups) != 0 || len(me.Roles) != 0 || len(me.Permissions) != 0 {
		t.Errorf("an unresolvable enrichment produced groups=%v roles=%v permissions=%v; a UserInfo outage must never hand out access", me.Groups, me.Roles, me.Permissions)
	}

	// And an expired token is still refused, whatever UserInfo would have
	// said about it.
	expired := func() string {
		header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "key-1"})
		payload, _ := json.Marshal(map[string]any{
			"sub": "ak-admin", "iss": provider.issuer, "aud": "bsystem-hub",
			"groups": []string{"BSYSTEM-Admins"}, "exp": time.Now().Add(-time.Hour).Unix(),
		})
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
		digest := sha256.Sum256([]byte(signed))
		signature, _ := rsa.SignPKCS1v15(rand.Reader, provider.key, crypto.SHA256, digest[:])
		return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
	}()
	if recorder := call(t, handler, http.MethodGet, "/api/v1/me", expired); recorder.Code != http.StatusUnauthorized {
		t.Errorf("an expired token got %d, want 401", recorder.Code)
	}
}

// No silent downgrade. A platform configured to validate locally must not fall
// back to asking UserInfo because a token did not parse — that would restore
// the network dependency it was configured to remove, reachable by anybody who
// sends something that is not a JWT.
func TestAnOpaqueTokenIsRefusedRatherThanSentToUserInfo(t *testing.T) {
	provider := newSigningProvider(t)
	_, handler := locallyValidatingApp(t, provider, map[string]map[string]any{
		"opaque-token": principal("ak-opaque", "BSYSTEM-Admins"),
	})
	// The identity fixture would happily accept this token. Local validation
	// must not consult it.
	recorder := call(t, handler, http.MethodGet, "/api/v1/me", "opaque-token")
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401: an opaque token was accepted by a platform validating locally (body: %s)", recorder.Code, recorder.Body.String())
	}
}

// A platform that cannot reach its provider at all has a different problem
// from a caller with a bad token, and the status code has to say so. Answering
// 401 sends every signed-in person to the login page during an outage that has
// nothing to do with their session.
func TestAnUnreachableProviderAnswers503RatherThan401(t *testing.T) {
	provider := newSigningProvider(t)
	token := provider.token(t, "ak-admin", []string{"BSYSTEM-Admins"})
	_, handler := locallyValidatingApp(t, provider, nil)
	provider.down.Store(true)

	recorder := call(t, handler, http.MethodGet, "/api/v1/me", token)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body: %s)", recorder.Code, recorder.Body.String())
	}
	// And the refusal says nothing about the provider's address.
	for _, secret := range []string{provider.issuer, "jwks", "127.0.0.1"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Errorf("the refusal names %q: %s", secret, recorder.Body.String())
		}
	}
}

// The default is unchanged. A deployment that has not configured an issuer
// still authenticates through UserInfo, which is the only thing that works
// with an opaque access token.
func TestWithoutAnIssuerTheUserInfoPathIsUnchanged(t *testing.T) {
	application, handler := integrationApp(t, map[string]map[string]any{
		"legacy": principal("ak-legacy", "BSYSTEM-Admins"),
	})
	if application.tokens != nil {
		t.Fatal("the fixture configured local validation without being asked to")
	}
	if recorder := call(t, handler, http.MethodGet, "/api/v1/me", "legacy"); recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
	if _, err := application.identify(context.Background(), "not-a-known-token"); err == nil {
		t.Error("an unknown token was accepted on the UserInfo path")
	}
}
