package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/httpx"
)

// Fake is a deterministic provider.
//
// It exists so the whole gateway — authorization, classification, redaction,
// audit, timeouts and limits — can be exercised end to end without a model to
// pay for or a credential to hold. That matters more than convenience: the
// parts of this package worth testing are the parts that decide what leaves
// the platform, and those must be testable without anything leaving it.
//
// It also records the last prompt it was given, which is how a test proves a
// credential never reached a provider payload rather than merely proving the
// redactor works in isolation.
type Fake struct {
	model string
	// Last is the prompt most recently received.
	Last string
	// Err, when set, is returned instead of an answer.
	Err error
}

// NewFake returns a fake provider.
func NewFake() *Fake { return &Fake{model: "fake-1"} }

func (f *Fake) Name() string  { return "fake" }
func (f *Fake) Model() string { return f.model }

func (f *Fake) Complete(ctx context.Context, prompt string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.Last = prompt
	if f.Err != nil {
		return "", f.Err
	}
	// The answer restates the shape of the input rather than inventing
	// content, so a test asserting on it is asserting on the gateway.
	lines := strings.Count(prompt, "\n- ")
	return fmt.Sprintf("fake answer from %d context fragment(s)", lines), nil
}

// ollamaRequest and ollamaResponse are the local model's wire shapes.
type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type ollamaResponse struct {
	Response string `json:"response"`
}

// Ollama talks to a local model server.
//
// Local is the preferred provider for anything above INTERNAL, because the
// prompt does not leave the deployment. Nothing here enforces that — it is a
// policy the gateway applies — but it is why this client exists before the
// cloud one.
type Ollama struct {
	client *httpx.Client
	model  string
}

// NewOllama builds a client. The base URL comes from configuration; there is
// no credential, because a local model server does not take one.
func NewOllama(config adapters.Config, model string) (*Ollama, error) {
	if strings.TrimSpace(config.BaseURL) == "" {
		return nil, errors.New("ai: Ollama base URL is required")
	}
	if model = strings.TrimSpace(model); model == "" {
		model = "llama3"
	}
	client, err := httpx.New(httpx.OptionsFor("ollama", config))
	if err != nil {
		return nil, err
	}
	return &Ollama{client: client, model: model}, nil
}

func (o *Ollama) Name() string  { return "ollama" }
func (o *Ollama) Model() string { return o.model }

func (o *Ollama) Complete(ctx context.Context, prompt string) (string, error) {
	var response ollamaResponse
	request := httpx.Request{
		Method: http.MethodPost,
		Path:   "/api/generate",
		Body:   ollamaRequest{Model: o.model, Prompt: prompt, Stream: false},
		// A completion is not idempotent in any useful sense — retrying
		// spends the same time again for a different answer — so it is not
		// marked retryable.
		Idempotent: false,
	}
	if err := o.client.Do(ctx, request, &response); err != nil {
		return "", providerError(err)
	}
	return strings.TrimSpace(response.Response), nil
}

// openAIRequest and openAIResponse are the cloud provider's wire shapes.
type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponse struct {
	Choices []struct {
		Message openAIMessage `json:"message"`
	} `json:"choices"`
}

// OpenAI talks to an OpenAI-compatible API.
//
// The credential is read from the environment and never rendered anywhere: it
// travels in a header the transport is documented never to log or put into an
// error. There is no configuration file, no default and no fallback — an
// unset key means the provider is not configured, which is different from
// being configured with an empty one.
type OpenAI struct {
	client *httpx.Client
	model  string
	apiKey string
}

// NewOpenAI builds a client from configuration. Both the base URL and the
// credential must be supplied; neither has a default, because a default
// endpoint with a missing key produces an authentication failure that reads
// like an outage.
func NewOpenAI(config adapters.Config, model string) (*OpenAI, error) {
	if strings.TrimSpace(config.BaseURL) == "" {
		return nil, errors.New("ai: OpenAI base URL is required")
	}
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("ai: OpenAI API key is required and must come from the environment")
	}
	if model = strings.TrimSpace(model); model == "" {
		return nil, errors.New("ai: OpenAI model is required")
	}
	client, err := httpx.New(httpx.OptionsFor("openai", config))
	if err != nil {
		return nil, err
	}
	return &OpenAI{client: client, model: model, apiKey: config.APIKey}, nil
}

func (o *OpenAI) Name() string  { return "openai" }
func (o *OpenAI) Model() string { return o.model }

func (o *OpenAI) Complete(ctx context.Context, prompt string) (string, error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+o.apiKey)

	var response openAIResponse
	request := httpx.Request{
		Method: http.MethodPost,
		Path:   "/v1/chat/completions",
		Header: header,
		Body: openAIRequest{
			Model:    o.model,
			Messages: []openAIMessage{{Role: "user", Content: prompt}},
		},
		Idempotent: false,
	}
	if err := o.client.Do(ctx, request, &response); err != nil {
		return "", providerError(err)
	}
	if len(response.Choices) == 0 {
		return "", fmt.Errorf("%w: no completion returned", ErrProviderUnavailable)
	}
	return strings.TrimSpace(response.Choices[0].Message.Content), nil
}

// providerError normalizes a transport failure, keeping the provider's status
// codes, hostnames and payloads out of anything a caller or a log can see. A
// model provider's error body can echo the prompt back.
func providerError(err error) error {
	var adapterErr *httpx.Error
	if errors.As(err, &adapterErr) {
		return fmt.Errorf("%w: %s", ErrProviderUnavailable, adapterErr.Kind)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("%w", ErrProviderUnavailable)
}
