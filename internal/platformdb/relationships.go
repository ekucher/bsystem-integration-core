package platformdb

// Generic cross-system relationships between Global IDs.
//
// This sits on top of global_entities rather than beside it: a relationship
// is a claim about two things the platform has already identified, never a
// second place identity gets minted. Looking one up must not allocate — that
// would make reading a relationship indistinguishable from asserting one —
// so every function here either resolves against rows that already exist or
// fails.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// relationVocabulary maps each canonical forward relation to the label its
// inverse is presented as. Only the forward label is ever stored; the
// inverse exists so a caller asking "what points at me" gets back a verb
// that reads naturally from their side, without a second row to keep in
// sync.
//
// "related-to" maps to itself because the relationship carries no direction
// in meaning, only in which endpoint happened to be the caller — both
// endpoints are already Global IDs, so there is no asymmetry left to
// describe.
var relationVocabulary = map[string]string{
	"related-to": "related-to",
	"tests":      "tested-by",
	"validates":  "validated-by",
	"documents":  "documented-by",
	"implements": "implemented-by",
	"depends-on": "required-by",
	"blocks":     "blocked-by",
	"references": "referenced-by",
}

// inverseVocabulary is the reverse index of relationVocabulary, built once
// so CreateRelationship can accept either the forward or the inverse label
// and still store a single canonical row.
var inverseVocabulary = func() map[string]string {
	reversed := make(map[string]string, len(relationVocabulary))
	for forward, inverse := range relationVocabulary {
		if forward == inverse {
			// "related-to" would otherwise overwrite itself with itself;
			// skipping it changes nothing but avoids the appearance of a
			// second, redundant entry.
			continue
		}
		reversed[inverse] = forward
	}
	return reversed
}()

// normalizeRelation resolves any of the sixteen labels a caller might use
// (eight forward, eight inverse) to the canonical forward label plus
// whether the caller's (from, to) pair needs to be swapped to store it.
//
// This is what makes "A tests B" and "B tested-by A" the same edge: without
// it, the two spellings would race to create two different rows for one
// fact, which is exactly the duplication requirement 4 rules out.
func normalizeRelation(label string) (canonical string, swap bool, ok bool) {
	if _, isForward := relationVocabulary[label]; isForward {
		return label, false, true
	}
	if forward, isInverse := inverseVocabulary[label]; isInverse {
		return forward, true, true
	}
	return "", false, false
}

// InverseRelation returns the label a relation reads as from the other
// endpoint. It is exported so read paths outside this package (list views,
// tests) can present a relationship correctly without duplicating the
// vocabulary.
func InverseRelation(canonical string) (string, bool) {
	inverse, ok := relationVocabulary[canonical]
	return inverse, ok
}

// RelationshipActor is who caused a relationship mutation.
//
// Both fields are optional individually but not together: at least one must
// be set. A user acting directly through the platform sets only UserGlobalID.
// A scheduled job or sync acting on its own sets only ServiceGlobalID. A
// user-triggered action carried out through a service — the shape most
// mutations in this platform actually take — sets both, so the row and its
// audit record can answer "who asked" and "what wrote it" separately instead
// of collapsing one into the other.
type RelationshipActor struct {
	UserGlobalID    string
	ServiceGlobalID string
	// RequestID carries X-Request-ID for the audit row and the outbox event,
	// mirroring how the HTTP layer threads it through WithCorrelationID
	// elsewhere. Optional.
	RequestID string
}

func (a RelationshipActor) validate() error {
	if a.UserGlobalID == "" && a.ServiceGlobalID == "" {
		return ErrMissingActor
	}
	return nil
}

// Relationship is one stored edge, always in its canonical forward
// direction.
type Relationship struct {
	ID                 int64     `json:"id"`
	FromGlobalID       string    `json:"from_global_id"`
	RelationType       string    `json:"relation_type"`
	ToGlobalID         string    `json:"to_global_id"`
	CreatedByUserID    string    `json:"created_by_user_id,omitempty"`
	CreatedByServiceID string    `json:"created_by_service_id,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

// RelationshipView is a relationship as seen from one of its endpoints: the
// label is whichever of the forward/inverse pair reads correctly from that
// side, and GlobalID is always the *other* endpoint.
type RelationshipView struct {
	GlobalID     string `json:"global_id"`
	RelationType string `json:"relation_type"`
	// Direction is "outgoing" when the queried Global ID is the row's
	// from_global_id (the label is the stored, forward one) and "incoming"
	// when it is the to_global_id (the label is the derived inverse). For
	// "related-to" the two are indistinguishable in meaning, but the field
	// is still reported accurately for whichever side stored the row.
	Direction string    `json:"direction"`
	CreatedAt time.Time `json:"created_at"`
}

var (
	// ErrMissingActor is returned when neither a human nor a service actor
	// was supplied. A relationship mutation with no provenance at all is not
	// something this platform will record.
	ErrMissingActor = errors.New("relationship requires a user or service actor")
	// ErrInvalidRelationType is returned for a label outside the Stage 1
	// vocabulary, forward or inverse.
	ErrInvalidRelationType = errors.New("unsupported relation_type")
	// ErrSelfRelationship is returned when both endpoints resolve to the same
	// Global ID, after any forward/inverse swap.
	ErrSelfRelationship = errors.New("a Global ID cannot relate to itself")
	// ErrUnknownGlobalID is returned when either endpoint does not already
	// exist in global_entities. CreateRelationship never allocates one to
	// make this pass.
	ErrUnknownGlobalID = errors.New("relationship endpoint is not a known Global ID")
)

// CreateRelationship asserts a typed edge between two existing Global IDs.
//
// label may be any of the eight forward verbs or their eight inverses; the
// pair is normalized to (canonical forward relation, from, to) before
// anything is written, so "A tests B" and "B tested-by A" always produce the
// same row. Calling this twice for the same edge — in either spelling — is
// idempotent: the second call reports created=false and returns the row the
// first call wrote, rather than erroring or duplicating it.
//
// Both endpoints must already be resolvable Global IDs; an unknown one is
// refused rather than minted, because allocating a Global ID as a side
// effect of describing a relationship is exactly the second identity system
// this store exists to avoid becoming.
func (db *DB) CreateRelationship(ctx context.Context, fromGlobalID, label, toGlobalID string, actor RelationshipActor) (Relationship, bool, error) {
	if err := actor.validate(); err != nil {
		return Relationship{}, false, err
	}
	canonical, swap, ok := normalizeRelation(label)
	if !ok {
		return Relationship{}, false, fmt.Errorf("%w: %q", ErrInvalidRelationType, label)
	}
	from, to := fromGlobalID, toGlobalID
	if swap {
		from, to = to, from
	}
	if from == "" || to == "" {
		return Relationship{}, false, fmt.Errorf("%w: endpoints must not be empty", ErrUnknownGlobalID)
	}
	if from == to {
		return Relationship{}, false, ErrSelfRelationship
	}

	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Relationship{}, false, err
	}
	defer tx.Rollback(ctx)

	var row Relationship
	var createdByUser, createdByService *string
	err = tx.QueryRow(ctx, `
INSERT INTO entity_relationships (from_global_id, relation_type, to_global_id, created_by_user_id, created_by_service_id)
VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''))
ON CONFLICT (from_global_id, relation_type, to_global_id) DO NOTHING
RETURNING id, from_global_id, relation_type, to_global_id, created_by_user_id, created_by_service_id, created_at`,
		from, canonical, to, actor.UserGlobalID, actor.ServiceGlobalID,
	).Scan(&row.ID, &row.FromGlobalID, &row.RelationType, &row.ToGlobalID, &createdByUser, &createdByService, &row.CreatedAt)

	created := true
	if errors.Is(err, pgx.ErrNoRows) {
		// The edge already existed. This is the normal, idempotent path a
		// second identical request takes, not a failure — mirroring how
		// CreateGlobalEntity treats a race on the same source record as a
		// lookup rather than an error.
		created = false
		err = tx.QueryRow(ctx, `
SELECT id, from_global_id, relation_type, to_global_id, created_by_user_id, created_by_service_id, created_at
FROM entity_relationships WHERE from_global_id=$1 AND relation_type=$2 AND to_global_id=$3`,
			from, canonical, to,
		).Scan(&row.ID, &row.FromGlobalID, &row.RelationType, &row.ToGlobalID, &createdByUser, &createdByService, &row.CreatedAt)
	}
	if err != nil {
		if isForeignKeyViolation(err) {
			return Relationship{}, false, fmt.Errorf("%w: %s or %s", ErrUnknownGlobalID, from, to)
		}
		return Relationship{}, false, err
	}
	if createdByUser != nil {
		row.CreatedByUserID = *createdByUser
	}
	if createdByService != nil {
		row.CreatedByServiceID = *createdByService
	}

	if created {
		// Audited and queued only on the path that actually wrote a row, for
		// the same reason CreateGlobalEntity only queues global_id.created
		// once: calling this twice for one edge must not read as two
		// mutations.
		auditEvent := AuditEvent{
			GlobalUserID: actor.UserGlobalID,
			Action:       "relationship.created",
			ResourceType: "relationship",
			ResourceID:   fmt.Sprintf("%d", row.ID),
			RequestID:    actor.RequestID,
			Metadata: map[string]any{
				"from_global_id":        row.FromGlobalID,
				"relation_type":         row.RelationType,
				"to_global_id":          row.ToGlobalID,
				"created_by_user_id":    actor.UserGlobalID,
				"created_by_service_id": actor.ServiceGlobalID,
			},
		}
		if _, err := tx.Exec(ctx, auditInsert, auditArgs(auditEvent)...); err != nil {
			return Relationship{}, false, fmt.Errorf("audit relationship creation: %w", err)
		}
		if err := queueDurableEvent(ctx, tx, "relationship.created", row.FromGlobalID, "", map[string]any{
			"id":                    row.ID,
			"from_global_id":        row.FromGlobalID,
			"relation_type":         row.RelationType,
			"to_global_id":          row.ToGlobalID,
			"created_by_user_id":    actor.UserGlobalID,
			"created_by_service_id": actor.ServiceGlobalID,
		}); err != nil {
			return Relationship{}, false, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Relationship{}, false, err
	}
	return row, created, nil
}

// LookupRelationship reports whether an edge exists, in either spelling,
// without creating or allocating anything. Read paths use this rather than
// CreateRelationship so that checking for a relationship can never
// accidentally assert one.
func (db *DB) LookupRelationship(ctx context.Context, fromGlobalID, label, toGlobalID string) (Relationship, bool, error) {
	canonical, swap, ok := normalizeRelation(label)
	if !ok {
		return Relationship{}, false, fmt.Errorf("%w: %q", ErrInvalidRelationType, label)
	}
	from, to := fromGlobalID, toGlobalID
	if swap {
		from, to = to, from
	}
	var row Relationship
	var createdByUser, createdByService *string
	err := db.pool.QueryRow(ctx, `
SELECT id, from_global_id, relation_type, to_global_id, created_by_user_id, created_by_service_id, created_at
FROM entity_relationships WHERE from_global_id=$1 AND relation_type=$2 AND to_global_id=$3`,
		from, canonical, to,
	).Scan(&row.ID, &row.FromGlobalID, &row.RelationType, &row.ToGlobalID, &createdByUser, &createdByService, &row.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Relationship{}, false, nil
	}
	if err != nil {
		return Relationship{}, false, err
	}
	if createdByUser != nil {
		row.CreatedByUserID = *createdByUser
	}
	if createdByService != nil {
		row.CreatedByServiceID = *createdByService
	}
	return row, true, nil
}

// ListRelationships returns every relationship touching globalID, from its
// point of view: relation_type is the forward label when globalID is the
// stored from_global_id, and the derived inverse label when globalID is the
// stored to_global_id. It never allocates and never mutates.
func (db *DB) ListRelationships(ctx context.Context, globalID string) ([]RelationshipView, error) {
	rows, err := db.pool.Query(ctx, `
SELECT to_global_id AS other, relation_type, created_at, 'outgoing' AS direction
FROM entity_relationships WHERE from_global_id=$1
UNION ALL
SELECT from_global_id AS other, relation_type, created_at, 'incoming' AS direction
FROM entity_relationships WHERE to_global_id=$1
ORDER BY created_at`, globalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := []RelationshipView{}
	for rows.Next() {
		var view RelationshipView
		var storedType string
		if err := rows.Scan(&view.GlobalID, &storedType, &view.CreatedAt, &view.Direction); err != nil {
			return nil, err
		}
		if view.Direction == "incoming" {
			if inverse, ok := InverseRelation(storedType); ok {
				storedType = inverse
			}
		}
		view.RelationType = storedType
		result = append(result, view)
	}
	return result, rows.Err()
}
