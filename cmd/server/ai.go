package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/ai"
	"github.com/ekucher/bsystem-integration-core/internal/authz"
	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
)

// aiSource is one record a caller asks the gateway to consider.
type aiSource struct {
	EntityType string `json:"entity_type"`
	GlobalID   string `json:"global_id"`
}

type aiRequestBody struct {
	Question string     `json:"question"`
	Sources  []aiSource `json:"sources,omitempty"`
}

type aiResponseBody struct {
	Answer         string `json:"answer"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Classification string `json:"classification"`
	// UsedSources lists what actually went into the prompt, which is not
	// always what was asked for: a caller may name a record they cannot read.
	UsedSources []string `json:"used_sources"`
	RequestID   string   `json:"request_id"`
}

// aiSourceRule says how one entity type is authorized and classified.
//
// The permission and scope are exactly what the equivalent read endpoint
// uses. That is the whole point: a question asked through the gateway must
// reach the same answer as the same person reading the record directly, or
// the gateway is a way around the rules rather than a consumer of them.
type aiSourceRule struct {
	Permission     string
	ScopeType      string
	Classification string
}

var aiSourceRules = map[string]aiSourceRule{
	// Customer data. A client or a contact identifies a person or a company
	// the platform is trusted with, so it is confidential wherever it goes.
	"client":  {Permission: "crm.client.read", ScopeType: authz.ScopeClient, Classification: ai.ClassConfidential},
	"contact": {Permission: "crm.client.read", ScopeType: authz.ScopeResource, Classification: ai.ClassConfidential},
	// Internal work.
	"project":  {Permission: "projects.task.read", ScopeType: authz.ScopeProject, Classification: ai.ClassInternal},
	"task":     {Permission: "projects.task.read", ScopeType: authz.ScopeResource, Classification: ai.ClassInternal},
	"document": {Permission: "wiki.document.read", ScopeType: authz.ScopeResource, Classification: ai.ClassInternal},
	"server":   {Permission: "operations.server.read", ScopeType: authz.ScopeResource, Classification: ai.ClassInternal},
	// An incident about a named customer carries that customer's situation,
	// so it takes the customer's classification rather than the platform's.
	"incident": {Permission: "support.incident.read", ScopeType: authz.ScopeClient, Classification: ai.ClassInternal},
}

func (a *app) aiAsk(w http.ResponseWriter, r *http.Request) {
	access := accessFrom(r.Context())
	started := time.Now()

	var body aiRequestBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	body.Question = strings.TrimSpace(body.Question)
	if body.Question == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "question is required"})
		return
	}
	if len(body.Question) > ai.MaxQuestionBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "question exceeds " + strconv.Itoa(ai.MaxQuestionBytes) + " bytes",
			"code":  "prompt_too_large",
		})
		return
	}
	if len(body.Sources) > ai.MaxSources {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "at most " + strconv.Itoa(ai.MaxSources) + " sources may be named",
			"code":  "too_many_sources",
		})
		return
	}

	requested := make([]string, 0, len(body.Sources))
	for _, source := range body.Sources {
		requested = append(requested, strings.TrimSpace(source.EntityType)+":"+strings.TrimSpace(source.GlobalID))
	}

	fragments, err := a.assembleContext(r, body.Sources)
	if err != nil {
		// An unreadable source is refused rather than dropped. Silently
		// answering from a smaller context would tell the caller the platform
		// had considered something it had not, and the answer would look like
		// it accounted for the record they named.
		var denial *aiDenial
		if errors.As(err, &denial) {
			a.auditAI(r, access, requested, nil, nil, "refused_unauthorized", 0, 0, started)
			writeJSON(w, denial.Status, denial.Body)
			return
		}
		logger.ErrorContext(r.Context(), "ai context assembly failed", "error", err.Error())
		a.auditAI(r, access, requested, nil, nil, "failed", 0, 0, started)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "context assembly unavailable"})
		return
	}

	prompt, err := ai.BuildPrompt(ai.Request{Question: body.Question, Fragments: fragments})
	switch {
	case errors.Is(err, ai.ErrCredentialContext):
		a.auditAI(r, access, requested, fragments, nil, "refused_credential", 0, 0, started)
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "the requested context is credential-classified and cannot be sent to a model",
			"code":  "credential_context",
		})
		return
	case errors.Is(err, ai.ErrPromptTooLarge):
		a.auditAI(r, access, requested, fragments, nil, "refused_too_large", 0, 0, started)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the assembled prompt is too large", "code": "prompt_too_large"})
		return
	case err != nil:
		logger.ErrorContext(r.Context(), "ai prompt assembly failed", "error", err.Error())
		a.auditAI(r, access, requested, fragments, nil, "failed", 0, 0, started)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "ai gateway unavailable"})
		return
	}

	// The provider call is bounded independently of the caller's own
	// deadline. A model that has not answered in this long will not answer
	// usefully, and a request left open is a request still costing money.
	ctx, cancel := context.WithTimeout(r.Context(), aiTimeout())
	defer cancel()

	answer, err := a.aiProvider.Complete(ctx, prompt)
	if err != nil {
		logger.ErrorContext(r.Context(), "ai completion failed", "provider", a.aiProvider.Name(), "error", err.Error())
		a.auditAI(r, access, requested, fragments, usedSources(fragments), "failed", len(prompt), 0, started)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "ai provider unavailable", "code": "ai_unavailable"})
		return
	}

	a.auditAI(r, access, requested, fragments, usedSources(fragments), "answered", len(prompt), len(answer), started)
	writeJSON(w, http.StatusOK, aiResponseBody{
		Answer:         answer,
		Provider:       a.aiProvider.Name(),
		Model:          a.aiProvider.Model(),
		Classification: ai.Classify(fragments),
		UsedSources:    usedSources(fragments),
		RequestID:      requestIDFrom(r.Context()),
	})
}

// aiDenial is an authorization refusal carried out of context assembly with
// the response it should produce.
type aiDenial struct {
	Status int
	Body   map[string]string
}

func (d *aiDenial) Error() string { return d.Body["error"] }

// assembleContext resolves and authorizes every named source.
//
// There is no search, no inference and no "related records". The caller names
// Global IDs and gets exactly those, each one authorized as though they had
// read it directly. A gateway that widened the context on the caller's behalf
// would be deciding what they may see, and that decision belongs to the
// evaluator.
func (a *app) assembleContext(r *http.Request, sources []aiSource) ([]ai.Fragment, error) {
	principal := principalFrom(r.Context())
	fragments := make([]ai.Fragment, 0, len(sources))

	for _, source := range sources {
		entityType := strings.ToLower(strings.TrimSpace(source.EntityType))
		globalID := strings.TrimSpace(source.GlobalID)

		rule, known := aiSourceRules[entityType]
		if !known {
			return nil, &aiDenial{Status: http.StatusBadRequest, Body: map[string]string{
				"error": "sources may only name " + strings.Join(aiSourceTypes(), ", "),
				"code":  "invalid_source",
			}}
		}

		fragment, scopeID, err := a.aiFragment(r, entityType, globalID, rule)
		if err != nil {
			return nil, err
		}

		decision, evalErr := a.authz.Evaluate(r.Context(), principal, rule.Permission, authz.Resource(rule.ScopeType, scopeID))
		if evalErr != nil {
			return nil, evalErr
		}
		if !decision.Allowed {
			// The refusal says which source was refused but nothing about it,
			// so a caller cannot use the gateway to probe for records.
			return nil, &aiDenial{Status: http.StatusForbidden, Body: map[string]string{
				"error": "not authorized to use " + globalID + " as context",
				"code":  "source_forbidden",
			}}
		}
		fragments = append(fragments, fragment)
	}
	return fragments, nil
}

// aiFragment loads the normalized summary of one record and reports the scope
// it should be authorized in.
//
// Only the platform's own normalized fields are used — a name, a title, a
// status. No upstream document body, description or payload is ever loaded:
// those are the parts most likely to contain something nobody decided to
// send to a model.
func (a *app) aiFragment(r *http.Request, entityType, globalID string, rule aiSourceRule) (ai.Fragment, string, error) {
	fragment := ai.Fragment{EntityType: entityType, GlobalID: globalID, Classification: rule.Classification}

	switch entityType {
	case "server":
		server, err := a.db.GetServer(r.Context(), globalID)
		if errors.Is(err, platformdb.ErrServerNotFound) {
			return ai.Fragment{}, "", notFoundSource(globalID)
		}
		if err != nil {
			return ai.Fragment{}, "", err
		}
		fragment.Title = server.Name
		fragment.Detail = "environment " + server.Environment + ", status " + server.Status
		scopeID := server.ID
		if server.ClientID != "" {
			// An owned server is authorized against its customer, matching
			// the server endpoint exactly.
			return fragment, server.ClientID, nil
		}
		return fragment, scopeID, nil

	case "incident":
		record, err := a.db.GetSupportRecord(r.Context(), globalID)
		if errors.Is(err, platformdb.ErrSupportRecordNotFound) {
			return ai.Fragment{}, "", notFoundSource(globalID)
		}
		if err != nil {
			return ai.Fragment{}, "", err
		}
		fragment.Title = record.Title
		fragment.Detail = record.Kind + ", severity " + record.Severity + ", status " + record.Status
		// The summary is deliberately not carried: it is where a reporter
		// pastes logs and occasionally a credential.
		if record.ClientID != "" {
			fragment.Classification = ai.ClassConfidential
			return fragment, record.ClientID, nil
		}
		return fragment, record.ID, nil

	default:
		// Upstream-backed entities are identified through the Global ID
		// mapping. Their names live in the source system, and fetching one
		// would make an AI question depend on an upstream being reachable;
		// the mapping is what the platform itself knows.
		entity, err := a.db.ResolveGlobalEntity(r.Context(), globalID)
		if err != nil || entity.EntityType != entityType {
			return ai.Fragment{}, "", notFoundSource(globalID)
		}
		fragment.Title = entity.GlobalID
		fragment.Detail = "recorded in " + entity.Source
		scopeID := entity.GlobalID
		if rule.ScopeType == authz.ScopeClient && entity.TenantID != "" {
			scopeID = entity.TenantID
		}
		return fragment, scopeID, nil
	}
}

// notFoundSource refuses a source the platform cannot resolve. It is the same
// answer an unauthorized source gets, so naming identifiers at the gateway
// discloses no more than naming them at the endpoint would.
func notFoundSource(globalID string) error {
	return &aiDenial{Status: http.StatusForbidden, Body: map[string]string{
		"error": "not authorized to use " + globalID + " as context",
		"code":  "source_forbidden",
	}}
}

func aiSourceTypes() []string {
	types := make([]string, 0, len(aiSourceRules))
	for entityType := range aiSourceRules {
		types = append(types, entityType)
	}
	return types
}

func usedSources(fragments []ai.Fragment) []string {
	used := make([]string, 0, len(fragments))
	for _, fragment := range fragments {
		used = append(used, fragment.GlobalID)
	}
	return used
}

// auditAI records the call. It is best effort: an audit write that failed
// must not turn an answered question into an error, but it is logged loudly
// because an unaudited AI call is exactly what the audit exists for.
func (a *app) auditAI(r *http.Request, access meResponse, requested []string, fragments []ai.Fragment, used []string, result string, promptBytes, answerBytes int, started time.Time) {
	err := a.db.InsertAIRequest(r.Context(), platformdb.AIRequest{
		ActorID:          access.ID,
		Provider:         a.aiProvider.Name(),
		Model:            a.aiProvider.Model(),
		RequestedSources: requested,
		EntityIDs:        used,
		Classification:   ai.Summarize(fragments),
		RequestID:        requestIDFrom(r.Context()),
		Result:           result,
		PromptBytes:      promptBytes,
		AnswerBytes:      answerBytes,
		DurationMS:       int(time.Since(started).Milliseconds()),
	})
	if err != nil {
		logger.ErrorContext(r.Context(), "ai audit write failed", "error", err.Error())
	}
	aiRequests.Inc(a.aiProvider.Name(), result)
}

func (a *app) aiAudit(w http.ResponseWriter, r *http.Request) {
	limit, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	if err != nil || limit <= 0 || limit > 500 {
		limit = 100
	}
	requests, err := a.db.ListAIRequests(r.Context(), limit)
	if err != nil {
		logger.ErrorContext(r.Context(), "ai audit listing failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "audit store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, requests)
}

// aiTimeout bounds one provider call.
func aiTimeout() time.Duration {
	return durationEnv("AI_TIMEOUT", ai.DefaultTimeout)
}

// aiProviderFromEnv builds the configured provider.
//
// The fake provider is the default. That is not a placeholder: it makes the
// gateway — authorization, classification, redaction, audit, bounds — usable
// and testable in every deployment without a credential to hold or a bill to
// pay, and a deployment that has not chosen a model has not chosen one.
//
// No credential is ever read from anywhere but the environment, and a
// misconfigured provider falls back to the fake with a warning rather than
// silently sending prompts somewhere unintended.
func aiProviderFromEnv() ai.Provider {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AI_PROVIDER"))) {
	case "ollama":
		config := adapterResilience()
		config.BaseURL = strings.TrimSpace(os.Getenv("OLLAMA_URL"))
		provider, err := ai.NewOllama(config, os.Getenv("OLLAMA_MODEL"))
		if err != nil {
			logger.Warn("ai provider disabled", "provider", "ollama", "error", err.Error())
			return ai.NewFake()
		}
		return provider
	case "openai":
		config := adapterResilience()
		config.BaseURL = strings.TrimSpace(os.Getenv("OPENAI_URL"))
		config.APIKey = os.Getenv("OPENAI_API_KEY")
		provider, err := ai.NewOpenAI(config, os.Getenv("OPENAI_MODEL"))
		if err != nil {
			// The error names what is missing, never the value of anything.
			logger.Warn("ai provider disabled", "provider", "openai", "error", err.Error())
			return ai.NewFake()
		}
		return provider
	default:
		return ai.NewFake()
	}
}
