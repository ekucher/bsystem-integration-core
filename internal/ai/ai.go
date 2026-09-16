// Package ai is the platform's gateway to language models.
//
// The whole package exists to enforce one sentence from the architecture: AI
// is an authorized consumer of BSYSTEM data, not a bypass around
// authorization. Everything here follows from that.
//
//   - A request names the Global IDs it wants. There is no query, no search
//     and no "answer from everything you know", because a gateway that
//     assembled its own context would be deciding what a caller may see, and
//     that decision belongs to the authorization evaluator.
//   - Every named source is authorized as if the caller had read it directly,
//     with the same permission and the same scope.
//   - Nothing classified CREDENTIAL is ever assembled, and anything that
//     looks like a credential is redacted before a provider sees it.
//
// A prompt sent to a provider leaves the platform. It may be logged by the
// provider, retained, or used for training; that is outside BSYSTEM's control
// entirely. So the rule is not "redact what we must" but "send the least that
// answers the question".
package ai

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Classification levels, matching the platform's security classification.
const (
	ClassPublic       = "PUBLIC"
	ClassInternal     = "INTERNAL"
	ClassConfidential = "CONFIDENTIAL"
	ClassSecret       = "SECRET"
	ClassCredential   = "CREDENTIAL"
)

// classOrder ranks the levels so a request's overall classification is the
// highest of its parts.
var classOrder = map[string]int{
	ClassPublic: 0, ClassInternal: 1, ClassConfidential: 2, ClassSecret: 3, ClassCredential: 4,
}

// Errors the gateway distinguishes.
var (
	// ErrCredentialContext means assembling the request would have put
	// credential-classified material in front of a model. It is a refusal,
	// not a redaction: the caller asked for something that cannot be answered
	// safely, and quietly answering a narrower question would hide that.
	ErrCredentialContext = errors.New("ai: credential-classified context refused")
	// ErrProviderUnavailable means the model could not be reached.
	ErrProviderUnavailable = errors.New("ai: provider unavailable")
	// ErrPromptTooLarge means the assembled prompt exceeds the bound.
	ErrPromptTooLarge = errors.New("ai: prompt exceeds the size limit")
)

// Bounds. They are here rather than in a provider because they protect the
// platform — from cost, from latency, and from a caller assembling a prompt
// large enough to be an exfiltration channel — not the provider.
const (
	// MaxQuestionBytes bounds what a caller may ask.
	MaxQuestionBytes = 4000
	// MaxPromptBytes bounds the whole assembled prompt.
	MaxPromptBytes = 24000
	// MaxSources bounds how many records one question may draw on.
	MaxSources = 20
	// DefaultTimeout bounds one provider call. A model that has not answered
	// in this long will not answer usefully.
	DefaultTimeout = 30 * time.Second
)

// Fragment is one authorized piece of context.
//
// It carries normalized platform data only — never an upstream payload. The
// platform's own summary of a record is what it has already decided is safe
// to show the caller; an upstream document is not.
type Fragment struct {
	EntityType string
	GlobalID   string
	// Title is the record's human label.
	Title string
	// Detail is a short normalized description. It is not a document body.
	Detail string
	// Classification is the level of the most sensitive thing in this
	// fragment.
	Classification string
}

// Request is one question with its authorized context.
type Request struct {
	Question  string
	Fragments []Fragment
	// Model selects within a provider. Empty takes the provider's default.
	Model string
}

// Response is a model's answer.
type Response struct {
	Answer   string
	Provider string
	Model    string
}

// Provider is a language model the gateway can call.
type Provider interface {
	// Name identifies the provider in audit records and errors.
	Name() string
	// Model is the model this provider is configured to use.
	Model() string
	// Complete answers a prompt. It must honour the context deadline.
	Complete(ctx context.Context, prompt string) (string, error)
}

// Classify returns the overall classification of a set of fragments, which is
// the highest of their parts. An empty set is PUBLIC: a question with no
// platform context in it discloses nothing of the platform's.
func Classify(fragments []Fragment) string {
	highest := ClassPublic
	for _, fragment := range fragments {
		if classOrder[fragment.Classification] > classOrder[highest] {
			highest = fragment.Classification
		}
	}
	return highest
}

// Summarize renders the classification of a request for the audit trail: the
// overall level and how many fragments sat at each. It deliberately records
// the shape of what was sent rather than the content.
func Summarize(fragments []Fragment) map[string]any {
	counts := map[string]int{}
	for _, fragment := range fragments {
		counts[fragment.Classification]++
	}
	return map[string]any{"overall": Classify(fragments), "by_class": counts}
}

// redactionMarker replaces anything that looks like a credential.
const redactionMarker = "[REDACTED]"

// credentialPatterns are the shapes a credential takes in text a human or an
// upstream might have written.
//
// The list is matched case-insensitively against a key-like prefix followed
// by a value. It is deliberately eager: a redacted field is an inconvenience,
// while a credential in a prompt has already left the platform by the time
// anyone notices.
var credentialKeys = []string{
	"authorization", "proxy-authorization", "x-api-key", "x-auth-token",
	"api_key", "apikey", "api-key", "secret", "client_secret", "password",
	"passwd", "pwd", "token", "access_token", "refresh_token", "id_token",
	"bearer", "private_key", "priv_key", "session", "cookie", "credential",
}

// Redact removes credential-shaped content from text.
//
// It handles three shapes: a key/value pair (`password: hunter2`), a bearer
// token in an Authorization header, and a PEM block. Anything it matches is
// replaced entirely rather than truncated, because a truncated secret is
// still a clue about the whole.
func Redact(text string) string {
	if text == "" {
		return text
	}
	redacted := redactPEM(text)

	lines := strings.Split(redacted, "\n")
	for i, line := range lines {
		lines[i] = redactLine(line)
	}
	return strings.Join(lines, "\n")
}

// redactLine replaces credential-shaped content on one line.
//
// Two passes, because credentials arrive in two shapes. A key/value pair is
// recognised by its key: everything after the separator goes, whatever it
// looks like, because "password: 1" is still a password. A pasted secret has
// no key at all — "my token is sk-live-..." — and is recognised by the shape
// of the value instead.
func redactLine(line string) string {
	if redacted, found := redactKeyedValue(line); found {
		return redacted
	}
	return redactSecretLookingWords(line)
}

// redactKeyedValue removes the value after a credential-shaped key.
func redactKeyedValue(line string) (string, bool) {
	lower := strings.ToLower(line)
	for _, key := range credentialKeys {
		index := strings.Index(lower, key)
		if index < 0 {
			continue
		}
		rest := line[index+len(key):]
		trimmed := strings.TrimLeft(rest, " \t")
		// The key must be followed by a separator, or it is a word that
		// merely contains it: "tokenizer" is not a token.
		if strings.HasPrefix(trimmed, ":") || strings.HasPrefix(trimmed, "=") {
			return line[:index+len(key)] + trimmed[:1] + " " + redactionMarker, true
		}
		// "Bearer <token>" and "Authorization <token>" carry the value with
		// no separator at all.
		if (key == "bearer" || key == "authorization") && rest != "" && (rest[0] == ' ' || rest[0] == '\t') && trimmed != "" {
			return line[:index+len(key)] + " " + redactionMarker, true
		}
	}
	return line, false
}

// secretPrefixes are issuer prefixes that identify a credential on sight.
var secretPrefixes = []string{
	"sk-", "sk_", "pk_", "rk_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_",
	"github_pat_", "xoxb-", "xoxp-", "xapp-", "glpat-", "AKIA", "ASIA",
	"eyJ", // a JWT header: base64 of the opening brace and quote.
}

// minSecretLength is the shortest run of mixed letters and digits treated as
// a secret on shape alone. Below it false positives outnumber credentials:
// an identifier, a version and a short hash all look alike.
const minSecretLength = 16

// redactSecretLookingWords removes words that look like credentials on their
// own, for the case where somebody pasted one into prose.
func redactSecretLookingWords(line string) string {
	fields := strings.Split(line, " ")
	changed := false
	for i, field := range fields {
		trimmed := strings.Trim(field, ".,;:!?()[]{}\"'")
		if trimmed == "" || !looksSecret(trimmed) {
			continue
		}
		fields[i] = strings.Replace(field, trimmed, redactionMarker, 1)
		changed = true
	}
	if !changed {
		return line
	}
	return strings.Join(fields, " ")
}

// looksSecret reports whether a word is credential-shaped.
//
// A known issuer prefix is decisive. Otherwise it takes a long run of mixed
// letters and digits, which is what a generated credential looks like and
// what ordinary prose does not: a Global ID is too short, a word has no
// digits, and a number has no letters.
func looksSecret(word string) bool {
	for _, prefix := range secretPrefixes {
		if strings.HasPrefix(word, prefix) && len(word) > len(prefix)+6 {
			return true
		}
	}
	if len(word) < minSecretLength {
		return false
	}
	var letters, digits int
	for _, r := range word {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letters++
		case r == '-' || r == '_' || r == '.' || r == '+' || r == '/' || r == '=':
			// Separators that generated credentials commonly contain.
		default:
			// Anything else — punctuation, a non-Latin letter — means this is
			// text rather than a generated value.
			return false
		}
	}
	return letters > 0 && digits > 0
}

// redactPEM removes private key blocks whole.
func redactPEM(text string) string {
	const begin = "-----BEGIN"
	const end = "-----END"
	for {
		start := strings.Index(text, begin)
		if start < 0 {
			return text
		}
		finish := strings.Index(text[start:], end)
		if finish < 0 {
			// An unterminated block is still a key. Drop the rest.
			return text[:start] + redactionMarker
		}
		tail := text[start+finish:]
		if lineEnd := strings.Index(tail, "\n"); lineEnd >= 0 {
			text = text[:start] + redactionMarker + tail[lineEnd:]
		} else {
			text = text[:start] + redactionMarker
		}
	}
}

// BuildPrompt assembles the text sent to a provider.
//
// It refuses rather than redacts when any fragment is credential-classified:
// the caller asked for something that cannot be answered safely, and quietly
// answering a narrower question would hide that from them.
//
// Everything that does go in is redacted, including the caller's own
// question — a user pasting a token into a prompt is the most likely way one
// reaches a model, and the gateway is the last place to catch it.
func BuildPrompt(request Request) (string, error) {
	for _, fragment := range request.Fragments {
		if fragment.Classification == ClassCredential {
			return "", ErrCredentialContext
		}
	}

	var builder strings.Builder
	builder.WriteString("You are answering a question about the BSYSTEM platform.\n")
	builder.WriteString("Use only the context below. If it does not answer the question, say so.\n\n")

	if len(request.Fragments) > 0 {
		builder.WriteString("Context:\n")
		for _, fragment := range request.Fragments {
			builder.WriteString("- ")
			builder.WriteString(fragment.EntityType)
			builder.WriteString(" ")
			builder.WriteString(fragment.GlobalID)
			builder.WriteString(": ")
			builder.WriteString(Redact(fragment.Title))
			if detail := Redact(fragment.Detail); detail != "" {
				builder.WriteString(" — ")
				builder.WriteString(detail)
			}
			builder.WriteString("\n")
		}
		builder.WriteString("\n")
	}

	builder.WriteString("Question: ")
	builder.WriteString(Redact(request.Question))
	builder.WriteString("\n")

	prompt := builder.String()
	if len(prompt) > MaxPromptBytes {
		return "", ErrPromptTooLarge
	}
	return prompt, nil
}
