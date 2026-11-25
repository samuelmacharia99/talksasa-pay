package webhook

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/reporting"
	"github.com/talksasa/talksasa-pay/internal/store"
)

var (
	// ErrNotFound: no delivery for that event id.
	ErrNotFound = errors.New("webhook: delivery not found")
	// ErrNotRedeliverable: only failed or skipped deliveries can be queued
	// again (a pending one is already queued; a delivered one is final).
	ErrNotRedeliverable = errors.New("webhook: only failed or skipped deliveries can be redelivered")
	// ErrInvalidFilter wraps a message safe to show to the admin user.
	ErrInvalidFilter = errors.New("webhook: invalid filter")
)

// Deliveries is the admin view of webhook delivery: list, detail, redeliver.
type Deliveries struct {
	pool  *db.Pool
	river *queue.Client
}

// NewDeliveries builds the admin delivery service. river may be an
// insert-only client (queue.NewInsertOnly).
func NewDeliveries(pool *db.Pool, river *queue.Client) *Deliveries {
	return &Deliveries{pool: pool, river: river}
}

// Delivery is one delivery summary row.
type Delivery struct {
	EventID        uuid.UUID
	AppID          uuid.UUID
	AccountPrefix  string
	EventType      string
	SubjectType    string
	SubjectID      uuid.UUID
	Status         store.WebhookDeliveryStatus
	Attempts       int32
	LastAttemptAt  *time.Time
	LastStatusCode *int32
	LastError      *string
	DeliveredAt    *time.Time
	CreatedAt      time.Time
}

// Detail is a delivery with its attempt log and the event body.
type Detail struct {
	Delivery       Delivery
	Attempts       []store.WebhookAttempt
	EventData      json.RawMessage
	EventCreatedAt time.Time
}

// Filter selects deliveries for the admin list.
type Filter struct {
	AppID  *uuid.UUID
	Status string // "" = any
	Cursor string
	Limit  int // default 50, max 200
}

// List returns deliveries newest first, keyset-paginated.
func (s *Deliveries) List(ctx context.Context, f Filter) (reporting.Page[Delivery], error) {
	var out reporting.Page[Delivery]
	var st store.NullWebhookDeliveryStatus
	if v := strings.TrimSpace(f.Status); v != "" {
		st = store.NullWebhookDeliveryStatus{WebhookDeliveryStatus: store.WebhookDeliveryStatus(v), Valid: true}
		if !st.WebhookDeliveryStatus.Valid() {
			return out, invalid("status must be pending, delivered, failed or skipped")
		}
	}
	at, id, err := decodeCursor(f.Cursor)
	if err != nil {
		return out, err
	}
	lim := int32(reporting.DefaultLimit)
	switch {
	case f.Limit > reporting.MaxLimit:
		lim = reporting.MaxLimit
	case f.Limit > 0:
		lim = int32(f.Limit)
	}
	rows, err := store.New(s.pool).ListWebhookDeliveriesWithApp(ctx, store.ListWebhookDeliveriesWithAppParams{
		AppID: f.AppID, Status: st, CursorAt: at, CursorID: id, Lim: lim + 1,
	})
	if err != nil {
		return out, err
	}
	if len(rows) > int(lim) {
		rows = rows[:lim]
		last := rows[len(rows)-1]
		out.NextCursor = encodeCursor(last.CreatedAt, last.EventID)
	}
	out.Items = make([]Delivery, len(rows))
	for i, r := range rows {
		out.Items[i] = Delivery(r)
	}
	return out, nil
}

// Get returns one delivery with every attempt and the event body.
func (s *Deliveries) Get(ctx context.Context, eventID uuid.UUID) (Detail, error) {
	return getDetail(ctx, store.New(s.pool), eventID)
}

func getDetail(ctx context.Context, q *store.Queries, eventID uuid.UUID) (Detail, error) {
	r, err := q.GetWebhookDeliveryWithEvent(ctx, eventID)
	if db.IsNotFound(err) {
		return Detail{}, ErrNotFound
	}
	if err != nil {
		return Detail{}, err
	}
	attempts, err := q.ListWebhookAttempts(ctx, eventID)
	if err != nil {
		return Detail{}, err
	}
	return Detail{
		Delivery: Delivery{
			EventID: r.EventID, AppID: r.AppID, AccountPrefix: r.AccountPrefix, EventType: r.EventType,
			SubjectType: r.SubjectType, SubjectID: r.SubjectID, Status: r.Status, Attempts: r.Attempts,
			LastAttemptAt: r.LastAttemptAt, LastStatusCode: r.LastStatusCode, LastError: r.LastError,
			DeliveredAt: r.DeliveredAt, CreatedAt: r.CreatedAt,
		},
		Attempts: attempts, EventData: json.RawMessage(r.EventData), EventCreatedAt: r.EventCreatedAt,
	}, nil
}

// redeliveryArgs is a WebhookArgs job (same kind, same worker) with an extra
// nonce. River's unique key covers the encoded args, and the original job may
// still sit in `completed` (it blocks an identical insert until River's
// cleaner removes it), so each redelivery needs distinct args. The worker
// ignores the extra field. Duplicate redeliveries are prevented by the
// delivery row instead: only a failed/skipped row can be requeued.
type redeliveryArgs struct {
	queue.WebhookArgs
	Redelivery uuid.UUID `json:"redelivery"`
}

// Redeliver puts a failed or skipped delivery back to pending and queues a
// fresh job (full retry budget), in one audited transaction.
func (s *Deliveries) Redeliver(ctx context.Context, actor audit.Actor, eventID uuid.UUID) (Delivery, error) {
	if s.river == nil {
		return Delivery{}, errors.New("webhook: redeliver needs a River client")
	}
	nonce, err := uuid.NewV7()
	if err != nil {
		return Delivery{}, err
	}
	var out Delivery
	err = db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, tx pgx.Tx) error {
		cur, err := q.LockWebhookDelivery(ctx, eventID)
		if db.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.Status != store.WebhookDeliveryStatusFailed && cur.Status != store.WebhookDeliveryStatusSkipped {
			return ErrNotRedeliverable
		}
		if _, err := q.RequeueWebhookDelivery(ctx, eventID); err != nil {
			return fmt.Errorf("webhook: requeue: %w", err)
		}
		if _, err := s.river.InsertTx(ctx, tx, redeliveryArgs{
			WebhookArgs: queue.WebhookArgs{EventID: eventID, AppID: cur.AppID}, Redelivery: nonce,
		}, nil); err != nil {
			return fmt.Errorf("webhook: insert job: %w", err)
		}
		d, err := getDetail(ctx, q, eventID)
		if err != nil {
			return err
		}
		out = d.Delivery
		pid := cur.AppID
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "webhook.redelivered", EntityType: "webhook_delivery", EntityID: eventID.String(), AppID: &pid,
			Details: map[string]any{"event_type": d.Delivery.EventType, "previous_status": string(cur.Status), "attempts": cur.Attempts},
		})
	})
	if err != nil {
		return Delivery{}, err
	}
	return out, nil
}

func invalid(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidFilter, msg) }

// Cursors use the same shape as the reporting lists: base64url("<unix µs>.<uuid>").
func encodeCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at.UnixMicro(), 10) + "." + id.String()))
}

func decodeCursor(c string) (*time.Time, *uuid.UUID, error) {
	if c == "" {
		return nil, nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || len(raw) > 100 {
		return nil, nil, invalid("malformed cursor")
	}
	us, idStr, ok := strings.Cut(string(raw), ".")
	n, err := strconv.ParseInt(us, 10, 64)
	if !ok || err != nil {
		return nil, nil, invalid("malformed cursor")
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, nil, invalid("malformed cursor")
	}
	t := time.UnixMicro(n).UTC()
	return &t, &id, nil
}
