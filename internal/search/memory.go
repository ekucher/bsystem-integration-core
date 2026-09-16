package search

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is an in-memory search provider.
//
// It exists so that search behaviour — matching, ranking, filtering,
// pagination and above all the authorization boundary — can be tested and
// exercised end to end without an engine to deploy. It is not a scalable
// index and does not pretend to be: it scans.
//
// Its ranking is deliberately simple and deterministic. A real engine ranks
// better, but a test that depends on a particular engine's scoring is a test
// of that engine. What the platform must guarantee is that the same query
// gives the same order, so a caller paging through results does not see one
// record twice and miss another.
type Memory struct {
	mu        sync.RWMutex
	documents map[string]Document
	// now is injectable so tests do not depend on wall-clock time.
	now func() time.Time
}

// NewMemory returns an empty in-memory provider.
func NewMemory() *Memory {
	return &Memory{documents: map[string]Document{}, now: time.Now}
}

func (m *Memory) Name() string { return "memory" }

// Index upserts documents by their Global ID.
func (m *Memory) Index(ctx context.Context, documents ...Document) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	normalized := make([]Document, 0, len(documents))
	for _, document := range documents {
		if err := document.Normalize(m.now()); err != nil {
			// Nothing is indexed when any document is invalid, so a batch
			// cannot be half-applied and leave the caller unable to say what
			// the index now holds.
			return err
		}
		normalized = append(normalized, document)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, document := range normalized {
		m.documents[document.ID] = document
	}
	return nil
}

// Delete removes documents. Removing one that is absent succeeds: a delete
// that failed when the record was already gone would turn a retry into an
// error.
func (m *Memory) Delete(ctx context.Context, ids ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		delete(m.documents, strings.TrimSpace(id))
	}
	return nil
}

// Len reports how many documents are indexed, for diagnostics and tests.
func (m *Memory) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.documents)
}

func (m *Memory) Search(ctx context.Context, query Query) (Page, error) {
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	terms := Tokenize(query.Text)
	if len(terms) == 0 {
		// No terms means no results, not every result.
		return Page{}, nil
	}
	types := map[string]bool{}
	for _, value := range query.Types {
		types[value] = true
	}
	permissions := map[string]bool{}
	for _, value := range query.Permissions {
		permissions[value] = true
	}

	m.mu.RLock()
	candidates := make([]Hit, 0, len(m.documents))
	for _, document := range m.documents {
		if len(types) > 0 && !types[document.Type] {
			continue
		}
		// The permission narrowing is an optimisation and a second line of
		// defence. The platform still evaluates every returned hit against
		// the authorization rules, because a provider is not where an
		// authorization decision belongs.
		if !query.AllPermissions && !anyPermission(document.Permissions, permissions) {
			continue
		}
		score := scoreOf(document, terms)
		if score <= 0 {
			continue
		}
		candidates = append(candidates, Hit{Document: document, Score: score})
	}
	m.mu.RUnlock()

	// Score first, then recency, then id. The final tiebreak on id is what
	// makes the order total: without it two equally scored documents could
	// swap between requests and a paging caller would see one twice.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		if !candidates[i].UpdatedAt.Equal(candidates[j].UpdatedAt) {
			return candidates[i].UpdatedAt.After(candidates[j].UpdatedAt)
		}
		return candidates[i].ID < candidates[j].ID
	})

	total := len(candidates)
	offset := query.Offset
	if offset > total {
		offset = total
	}
	window := candidates[offset:]
	limit := query.Limit
	if limit > 0 && limit < len(window) {
		window = window[:limit]
	}
	return Page{Hits: window, Total: total, Examined: len(window)}, nil
}

func anyPermission(required []string, held map[string]bool) bool {
	for _, permission := range required {
		if held[permission] {
			return true
		}
	}
	return false
}

// scoreOf ranks a document against the query terms.
//
// Every term must appear somewhere, so a two-word query does not match a
// document containing only one of them. A title match counts for more than a
// summary match, and an exact term counts for more than a prefix, which is
// enough to put the obvious answer first without pretending to be a ranking
// engine.
func scoreOf(document Document, terms []string) float64 {
	title := Tokenize(document.Title)
	summary := Tokenize(document.Summary)
	var score float64
	for _, term := range terms {
		termScore := fieldScore(title, term) * 2
		if summaryScore := fieldScore(summary, term); summaryScore > 0 {
			termScore += summaryScore
		}
		if termScore == 0 {
			return 0
		}
		score += termScore
	}
	return score
}

func fieldScore(tokens []string, term string) float64 {
	best := 0.0
	for _, token := range tokens {
		switch {
		case token == term:
			return 2
		case strings.HasPrefix(token, term):
			best = 1
		}
	}
	return best
}
