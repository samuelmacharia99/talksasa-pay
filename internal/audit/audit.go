// Package audit records sensitive actions in the hash-chained audit_log.
// Entries are written inside the same database transaction as the action
// they describe, so an action and its audit record commit or fail together.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/platform/log"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// Actor identifies who performed an action.
type Actor struct {
	Type store.AuditActorType
	ID   string // admin user id, CLI operator name, product id, or system component
}

// String renders the actor for transaction_events ("cli:alice").
func (a Actor) String() string { return string(a.Type) + ":" + a.ID }

// Validate rejects anonymous actors.
func (a Actor) Validate() error {
	if !a.Type.Valid() || strings.TrimSpace(a.ID) == "" {
		return errors.New("audit: actor type and id are required")
	}
	return nil
}

// System returns an actor for an internal component (worker, job).
func System(component string) Actor { return Actor{Type: store.AuditActorTypeSystem, ID: component} }

// CLI returns an actor for an operator using the admin CLI.
func CLI(operator string) Actor { return Actor{Type: store.AuditActorTypeCli, ID: operator} }

// Admin returns an actor for a signed-in admin panel user.
func Admin(userID uuid.UUID) Actor {
	return Actor{Type: store.AuditActorTypeAdmin, ID: userID.String()}
}

// Product returns an actor for an authenticated product API key.
func Product(productID uuid.UUID) Actor {
	return Actor{Type: store.AuditActorTypeProduct, ID: productID.String()}
}

// Entry is one audited action.
type Entry struct {
	Actor      Actor
	Action     string // dotted, e.g. "product.created", "api_key.revoked"
	EntityType string
	EntityID   string
	ProductID  *uuid.UUID
	Details    map[string]any
}

// Record appends an entry. Details must never contain secrets; keys that look
// sensitive are rejected outright rather than silently stored.
func Record(ctx context.Context, q *store.Queries, e Entry) error {
	if err := e.Actor.Validate(); err != nil {
		return err
	}
	for k := range e.Details {
		if log.IsSensitiveKey(k) {
			return fmt.Errorf("audit: refusing to store sensitive detail key %q", k)
		}
	}
	details := []byte("{}")
	if len(e.Details) > 0 {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return fmt.Errorf("audit: encode details: %w", err)
		}
		details = b
	}
	var entityID *string
	if e.EntityID != "" {
		entityID = &e.EntityID
	}
	return q.InsertAudit(ctx, store.InsertAuditParams{
		ActorType:  e.Actor.Type,
		ActorID:    e.Actor.ID,
		Action:     e.Action,
		EntityType: e.EntityType,
		EntityID:   entityID,
		ProductID:  e.ProductID,
		Details:    details,
	})
}
