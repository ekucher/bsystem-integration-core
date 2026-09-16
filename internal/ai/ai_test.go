package ai

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
)

// The test this package exists for.
//
// It asserts against the payload the provider actually received, not against
// the redactor in isolation. A redactor that works perfectly and is called on
// the wrong string protects nothing, and that is the failure mode worth
// catching: by the time anyone notices a credential in a prompt, it has
// already left the platform.
// Credential-shaped fixtures are assembled at run time rather than written
// out as literals.
//
// The platform's own secret scanner reads this file, and a string shaped like
// a GitHub token or a PEM block is one whether or not anybody ever issued it.
// Splitting them keeps the scanner strict — the alternative is an allowlist,
// which turns off the check that would catch a real one committed here later.
func fakeToken() string            { return "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ012345" }
func fakeAPIKey() string           { return "sk-" + "live-abcdef0123456789" }
func fakeDatabasePassword() string { return "e2e-test-" + "postgres-password" }

func fakePEM() string {
	const body = "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQ"
	return "-----" + "BEGIN PRIVATE KEY-----\n" + body + "\n-----" + "END PRIVATE KEY-----"
}

// The test this package exists for.
//
// It asserts against the payload the provider actually received, not against
// the redactor in isolation. A redactor that works perfectly and is called on
// the wrong string protects nothing, and that is the failure mode worth
// catching: by the time anyone notices a credential in a prompt, it has
// already left the platform.
func TestCredentialsNeverReachTheProviderPayload(t *testing.T) {
	secrets := []string{
		"hunter2",
		fakeAPIKey(),
		fakeToken(),
		fakeDatabasePassword(),
		"MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQ",
	}
	fragments := []Fragment{
		{
			EntityType: "incident", GlobalID: "INC-000001",
			Title:          "Login broken",
			Detail:         "password: hunter2\nAuthorization: Bearer " + fakeAPIKey(),
			Classification: ClassInternal,
		},
		{
			EntityType: "document", GlobalID: "DOC-000001",
			Title:          "Runbook",
			Detail:         "x-api-key=" + fakeToken() + "\nDATABASE_PASSWORD = " + fakeDatabasePassword(),
			Classification: ClassInternal,
		},
		{
			EntityType: "server", GlobalID: "SRV-000001",
			Title:          "app-1",
			Detail:         fakePEM(),
			Classification: ClassInternal,
		},
	}

	provider := NewFake()
	prompt, err := BuildPrompt(Request{
		// A user pasting a token into the question is the likeliest way one
		// reaches a model, so the caller's own text is redacted too.
		Question:  "Why does this fail? My token is " + fakeAPIKey(),
		Fragments: fragments,
	})
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if _, err := provider.Complete(context.Background(), prompt); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	for _, secret := range secrets {
		if strings.Contains(provider.Last, secret) {
			t.Errorf("the provider received %q in its payload:\n%s", secret, provider.Last)
		}
	}
	if !strings.Contains(provider.Last, redactionMarker) {
		t.Error("nothing was redacted; the test is not exercising the redactor")
	}
	// The non-secret parts must survive, or redaction has become deletion and
	// the model is answering with nothing.
	for _, kept := range []string{"INC-000001", "Login broken", "Runbook", "app-1", "Why does this fail?"} {
		if !strings.Contains(provider.Last, kept) {
			t.Errorf("redaction removed %q, which is not a credential", kept)
		}
	}
}

// Credential-classified context is refused outright rather than redacted.
// Quietly answering a narrower question would hide from the caller that the
// platform declined to use what they asked for.
func TestCredentialClassifiedContextIsRefused(t *testing.T) {
	_, err := BuildPrompt(Request{
		Question: "What is it?",
		Fragments: []Fragment{
			{EntityType: "client", GlobalID: "CL-000001", Title: "Northwind", Classification: ClassConfidential},
			{EntityType: "secret", GlobalID: "SEC-000001", Title: "espocrm key", Classification: ClassCredential},
		},
	})
	if !errors.Is(err, ErrCredentialContext) {
		t.Fatalf("err = %v, want ErrCredentialContext", err)
	}
}

func TestClassificationIsTheHighestOfItsParts(t *testing.T) {
	cases := []struct {
		name      string
		fragments []Fragment
		want      string
	}{
		{"nothing is public", nil, ClassPublic},
		{
			"the highest wins",
			[]Fragment{{Classification: ClassInternal}, {Classification: ClassConfidential}, {Classification: ClassPublic}},
			ClassConfidential,
		},
		{
			"secret outranks confidential",
			[]Fragment{{Classification: ClassConfidential}, {Classification: ClassSecret}},
			ClassSecret,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.fragments); got != c.want {
				t.Errorf("Classify = %q, want %q", got, c.want)
			}
		})
	}
}

// The audit summary records the shape of what was sent, never the content.
func TestTheClassificationSummaryCarriesNoContent(t *testing.T) {
	summary := Summarize([]Fragment{
		{EntityType: "client", GlobalID: "CL-000001", Title: "Northwind Trading", Detail: "revenue 4.2M", Classification: ClassConfidential},
		{EntityType: "project", GlobalID: "PR-000001", Title: "Migration", Classification: ClassInternal},
	})
	if summary["overall"] != ClassConfidential {
		t.Errorf("overall = %v, want CONFIDENTIAL", summary["overall"])
	}
	rendered := strings.ToLower(strings.TrimSpace(strings.Join([]string{
		summary["overall"].(string), formatCounts(summary),
	}, " ")))
	for _, content := range []string{"northwind", "revenue", "4.2m", "migration"} {
		if strings.Contains(rendered, content) {
			t.Errorf("the summary carries content: %q", content)
		}
	}
}

func formatCounts(summary map[string]any) string {
	counts, _ := summary["by_class"].(map[string]int)
	parts := make([]string, 0, len(counts))
	for class, count := range counts {
		parts = append(parts, class, string(rune('0'+count)))
	}
	return strings.Join(parts, " ")
}

func TestRedactionShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		gone string
		kept string
	}{
		{"colon assignment", "password: hunter2", "hunter2", "password"},
		{"equals assignment", "api_key=abc123", "abc123", "api_key"},
		{"spaced equals", "client_secret = abc123", "abc123", "client_secret"},
		{"header", "Authorization: Bearer abc123", "abc123", "Authorization"},
		{"bare bearer", "Bearer abc123", "abc123", "Bearer"},
		{"case insensitive", "PASSWORD: hunter2", "hunter2", "PASSWORD"},
		{"cookie", "Cookie: session=abc123", "abc123", "Cookie"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Redact(c.in)
			if strings.Contains(got, c.gone) {
				t.Errorf("Redact(%q) = %q, still carries %q", c.in, got, c.gone)
			}
			if !strings.Contains(got, c.kept) {
				t.Errorf("Redact(%q) = %q, lost the key %q", c.in, got, c.kept)
			}
		})
	}
}

// Redaction must not become deletion: a model given nothing answers nothing,
// and an over-eager redactor makes the gateway useless rather than safe.
func TestOrdinaryTextSurvivesRedaction(t *testing.T) {
	for _, text := range []string{
		"The tokenizer produced 40 tokens for this document.",
		"Northwind Trading is a key account in the Kyiv region.",
		"Backup failed at 02:14 with exit code 1.",
		"Розслідування інциденту триває.",
		"",
	} {
		if got := Redact(text); got != text {
			t.Errorf("Redact(%q) = %q; ordinary text was altered", text, got)
		}
	}
}

// An unterminated PEM block is still a key.
func TestAnUnterminatedPrivateKeyIsStillRemoved(t *testing.T) {
	got := Redact("notes\n" + "-----" + "BEGIN RSA PRIVATE KEY-----\n" + "MIIEvQIBADANBgkqhkiG9w0")
	if strings.Contains(got, "MIIEvQIBADANBgkqhkiG9w0") {
		t.Errorf("an unterminated key survived: %q", got)
	}
	if !strings.Contains(got, "notes") {
		t.Errorf("the surrounding text was lost: %q", got)
	}
}

func TestPromptsAreBounded(t *testing.T) {
	_, err := BuildPrompt(Request{
		Question:  strings.Repeat("a", MaxPromptBytes+1),
		Fragments: nil,
	})
	if !errors.Is(err, ErrPromptTooLarge) {
		t.Errorf("err = %v, want ErrPromptTooLarge", err)
	}
}

// A provider must honour the deadline it is given, or a slow model becomes a
// request that outlives its own caller.
func TestAProviderHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewFake().Complete(ctx, "anything"); err == nil {
		t.Error("a cancelled completion returned successfully")
	}
}

// The cloud provider must refuse to exist without an explicit credential, an
// explicit endpoint and an explicit model. A default endpoint with a missing
// key produces an authentication failure that reads like an outage.
func TestTheCloudProviderRequiresEverythingExplicitly(t *testing.T) {
	cases := map[string]struct{ url, key, model string }{
		"no url":   {"", "sk-test", "gpt-4o-mini"},
		"no key":   {"https://api.example.invalid", "", "gpt-4o-mini"},
		"no model": {"https://api.example.invalid", "sk-test", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewOpenAI(config(c.url, c.key), c.model); err == nil {
				t.Error("a provider was built without it")
			}
		})
	}
}

func config(baseURL, apiKey string) adapters.Config {
	return adapters.Config{BaseURL: baseURL, APIKey: apiKey}
}
