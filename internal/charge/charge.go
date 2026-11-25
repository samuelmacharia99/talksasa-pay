// Package charge manages charges: an app's request to collect an exact
// amount ("collect KES X from this phone for my reference Y"). Amounts are
// always set here, server-side, from the app's API call; nothing a
// customer's browser or phone sends can change them.
//
// Money rules (gateway plan, approved 2026-11-25):
//  1. amount is whole KES, 1 to 250,000, and never changes (cancel and
//     create a new charge instead); reference is the app's own, unique per
//     app (duplicate -> ErrDuplicateReference, 409); metadata is a JSON
//     object of at most 4 KB, echoed back; expires_in is 15 min to 7 days
//     (default 24 h).
//  2. every charge has an account number: the app's own (must start with
//     its prefix, accountref.Validate) or generated (accountref.Generate).
//  4. a failed prompt does not fail the charge.
//
// Paying a charge is ledger.SucceedCharge's job (internal/ledger), driven by
// M-Pesa callbacks (internal/stk); nothing here records money.
package charge

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/accountref"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/phone"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/stk"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// Limits (money rule 1).
const (
	MinAmount        = 1
	MaxAmount        = 250_000
	MaxReferenceLen  = 128
	MaxDescription   = 500
	MaxMetadataBytes = 4096
	MinExpiresIn     = 15 * time.Minute
	MaxExpiresIn     = 7 * 24 * time.Hour
	DefaultExpiresIn = 24 * time.Hour
)

// Errors (mapped to stable API problem codes by internal/appapi).
var (
	ErrNotFound             = errors.New("charge: not found")                                                     // 404 not_found
	ErrDuplicateReference   = errors.New("charge: a charge with this reference already exists")                   // 409 duplicate_reference
	ErrNotCancellable       = errors.New("charge: only pending charges can be cancelled")                         // 409 not_cancellable
	ErrIdempotencyConflict  = errors.New("charge: idempotency key reused with different parameters")              // 409 idempotency_conflict
	ErrAccountNumberInUse   = errors.New("charge: a live charge already uses this generated account number")      // retried internally
	ErrAppNotActive         = errors.New("charge: app is not active")                                             // 403 app_suspended
	ErrInvalidAmount        = errors.New("charge: amount must be a whole number of KES from 1 to 250000")         // 422 amount
	ErrInvalidReference     = errors.New("charge: reference must be 1-128 characters")                            // 422 reference
	ErrInvalidDescription   = errors.New("charge: description must be 1-500 characters")                          // 422 description
	ErrInvalidMetadata      = errors.New("charge: metadata must be a JSON object of at most 4096 bytes")          // 422 metadata
	ErrInvalidAccountNumber = errors.New("charge: account number must be PREFIX-, a digit, then A-Z/0-9, max 12") // 422 account_number
	ErrInvalidPhone         = errors.New("charge: phone must be a Kenyan Safaricom number")                       // 422 phone
	ErrInvalidExpiry        = errors.New("charge: expires_in must be from 900 to 604800 seconds")                 // 422 expires_in
	ErrInvalidIdempotency   = errors.New("charge: idempotency key must be 1-255 printable characters")            // 400 Idempotency-Key
	ErrInvalidCursor        = errors.New("charge: invalid list cursor")                                           // 400 cursor
	ErrInvalidStatus        = errors.New("charge: unknown status filter")                                         // 400 status
)

// ErrNotImplemented reported the gateway-core stub sentinel. Every method
// is implemented now, so it is always false; kept for existing callers.
//
// Deprecated: nothing returns a not-implemented error any more.
func ErrNotImplemented(error) bool { return false }

// Charge is a stored charge.
type Charge = store.Charge

// Push is a stored STK push.
type Push = store.StkPush

// CreateParams are the app-supplied fields of POST /v1/charges.
type CreateParams struct {
	Reference     string
	Amount        int64
	Description   string          // optional
	Metadata      json.RawMessage // optional JSON object, <= 4 KB
	AccountNumber string          // optional; generated when empty
	Phone         string          // optional; when set, the first prompt is sent there
	ExpiresIn     time.Duration   // 0 = DefaultExpiresIn
	// IdempotencyKey is the Idempotency-Key header (required). The same key
	// with the same parameters replays the original charge.
	IdempotencyKey string
}

// Created is the outcome of Create.
type Created struct {
	Charge   Charge
	Replayed bool // same idempotency key and parameters: the original charge
	// Push is the first prompt when a phone was given (nil otherwise, or
	// when sending failed before a push was stored).
	Push *Push
	// PushErr is why the first prompt could not be sent (the charge itself
	// was still created and can be prompted again or paid by paybill).
	PushErr error
}

// ListParams filter GET /v1/charges. Cursor paging is by (created_at, id)
// descending.
type ListParams struct {
	Status    *store.ChargeStatus
	Reference string
	CursorAt  *time.Time
	CursorID  *uuid.UUID
	Limit     int // 1-100
}

// Cancelled is the outcome of Cancel.
type Cancelled struct {
	Charge Charge
	// AlreadyCancelled: an idempotent retry; nothing changed.
	AlreadyCancelled bool
}

// Prompter sends STK prompts (*stk.Service).
type Prompter interface {
	Send(ctx context.Context, actor audit.Actor, p stk.SendParams) (stk.Push, bool, error)
}

var _ Prompter = (*stk.Service)(nil)

// Options configure the service.
type Options struct {
	Pool   *db.Pool
	Events *events.Emitter
	// STK sends the first prompt when a charge has a phone. nil: M-Pesa off
	// (the charge is created; Created.PushErr is stk.ErrUnavailable).
	STK    Prompter
	Logger *slog.Logger
	Now    func() time.Time // nil = time.Now
}

// Service manages charges. Safe for concurrent use.
type Service struct {
	o Options
}

// NewService builds the service.
func NewService(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Service{o: o}
}

// Create creates a charge (idempotent on app + IdempotencyKey) and, when a
// phone is given, sends the first prompt after the charge commits. If that
// prompt cannot be sent the charge still exists: Created.PushErr says why
// (the app can prompt again, or the customer pays at the paybill).
//
// Replays (same key, same parameters) return the original charge with
// Replayed=true and its first prompt, without prompting again. The same key
// with different parameters is ErrIdempotencyConflict.
func (s *Service) Create(ctx context.Context, actor audit.Actor, appID uuid.UUID, p CreateParams) (Created, error) {
	if err := actor.Validate(); err != nil {
		return Created{}, err
	}
	v, err := validate(p)
	if err != nil {
		return Created{}, err
	}
	q := store.New(s.o.Pool)
	if existing, err := q.GetChargeByIdempotencyKey(ctx, store.GetChargeByIdempotencyKeyParams{AppID: appID, IdempotencyKey: v.key}); err == nil {
		return s.replay(ctx, existing, v.hash)
	} else if !db.IsNotFound(err) {
		return Created{}, err
	}
	a, err := q.GetApp(ctx, appID)
	if db.IsNotFound(err) {
		return Created{}, ErrAppNotActive
	}
	if err != nil {
		return Created{}, err
	}
	if a.Status != store.AppStatusActive {
		return Created{}, ErrAppNotActive
	}
	if v.account != "" {
		if err := accountref.Validate(a.AccountPrefix, v.account); err != nil {
			return Created{}, ErrInvalidAccountNumber
		}
	}

	var c Charge
	for attempt := 0; ; attempt++ {
		c, err = s.insert(ctx, actor, a, v)
		if errors.Is(err, ErrAccountNumberInUse) && v.account == "" && attempt < 5 {
			continue // generated number collided with a live charge: draw again
		}
		break
	}
	if errors.Is(err, errKeyTaken) {
		// A concurrent request with the same key won the insert.
		existing, gerr := q.GetChargeByIdempotencyKey(ctx, store.GetChargeByIdempotencyKeyParams{AppID: appID, IdempotencyKey: v.key})
		if gerr != nil {
			return Created{}, gerr
		}
		return s.replay(ctx, existing, v.hash)
	}
	if err != nil {
		return Created{}, err
	}
	out := Created{Charge: c}
	if v.phone == "" {
		return out, nil
	}
	if s.o.STK == nil {
		out.PushErr = stk.ErrUnavailable
		return out, nil
	}
	push, _, perr := s.o.STK.Send(ctx, actor, stk.SendParams{
		AppID: appID, ChargeID: c.ID, Phone: v.phone, Source: stk.SourceApp, IdempotencyKey: firstPushKey(c.ID),
	})
	if push.ID != uuid.Nil {
		out.Push = &push // also a rejected prompt (status failed)
	}
	if perr != nil {
		out.PushErr = perr
		s.o.Logger.WarnContext(ctx, "charge created but its first prompt was not sent",
			slog.String("charge_id", c.ID.String()), slog.String("error", perr.Error()))
	}
	return out, nil
}

// errKeyTaken: InsertCharge found the idempotency key already used.
var errKeyTaken = errors.New("charge: idempotency key taken")

func (s *Service) insert(ctx context.Context, actor audit.Actor, a store.App, v validated) (Charge, error) {
	account, generated := v.account, false
	if account == "" {
		g, err := accountref.Generate(a.AccountPrefix)
		if err != nil {
			return Charge{}, err
		}
		account, generated = g, true
	}
	token, err := hostedToken()
	if err != nil {
		return Charge{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Charge{}, err
	}
	var c Charge
	err = db.WithTx(ctx, s.o.Pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		var err error
		c, err = q.InsertCharge(ctx, store.InsertChargeParams{
			ID: id, AppID: a.ID, Reference: v.reference, Description: v.description, Amount: v.amount,
			Metadata: v.metadata, AccountNumber: account, AccountNumberNorm: accountref.Normalize(account),
			AccountGenerated: generated, Phone: optStr(v.phone), HostedToken: token,
			IdempotencyKey: v.key, RequestHash: v.hash, ExpiresAt: s.o.Now().Add(v.expiresIn),
		})
		if db.IsNotFound(err) {
			return errKeyTaken
		}
		if err != nil {
			return err
		}
		details := map[string]any{"reference": v.reference, "amount": v.amount, "account_number": account}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "charge.created", EntityType: "charge", EntityID: c.ID.String(), AppID: &a.ID, Details: details,
		})
	})
	switch {
	case db.IsUniqueViolation(err, "charges_app_id_reference_key"):
		return Charge{}, ErrDuplicateReference
	case db.IsUniqueViolation(err, "charges_generated_account_live"):
		return Charge{}, ErrAccountNumberInUse
	case db.IsUniqueViolation(err, "charges_app_id_idempotency_key_key"):
		return Charge{}, errKeyTaken
	case db.IsCheckViolation(err, "charges_metadata_check"):
		return Charge{}, ErrInvalidMetadata // jsonb's text form is larger than what was sent
	}
	return c, err
}

// replay returns the charge stored under an idempotency key if it was
// created with the same parameters.
func (s *Service) replay(ctx context.Context, c Charge, hash []byte) (Created, error) {
	if !bytes.Equal(c.RequestHash, hash) {
		return Created{}, ErrIdempotencyConflict
	}
	out := Created{Charge: c, Replayed: true}
	key := firstPushKey(c.ID)
	push, err := store.New(s.o.Pool).GetSTKPushByIdempotencyKey(ctx, store.GetSTKPushByIdempotencyKeyParams{AppID: c.AppID, IdempotencyKey: &key})
	switch {
	case err == nil:
		out.Push = &push
	case !db.IsNotFound(err):
		return Created{}, err
	}
	return out, nil
}

// firstPushKey is the idempotency key of the prompt Create sends, so a
// retried Create can never prompt twice.
func firstPushKey(chargeID uuid.UUID) string { return "charge-create:" + chargeID.String() }

// validated is CreateParams after validation and normalisation.
type validated struct {
	reference   string
	amount      int64
	description *string
	metadata    []byte
	account     string // "" = generate
	phone       string // normalised, "" = none
	expiresIn   time.Duration
	key         string
	hash        []byte
}

func validate(p CreateParams) (validated, error) {
	var v validated
	v.key = strings.TrimSpace(p.IdempotencyKey)
	if v.key == "" || len(v.key) > 255 || !printable(v.key) {
		return v, ErrInvalidIdempotency
	}
	if p.Amount < MinAmount || p.Amount > MaxAmount {
		return v, ErrInvalidAmount
	}
	v.amount = p.Amount
	v.reference = strings.TrimSpace(p.Reference)
	if v.reference == "" || utf8.RuneCountInString(v.reference) > MaxReferenceLen || !printable(v.reference) {
		return v, ErrInvalidReference
	}
	if d := strings.TrimSpace(p.Description); d != "" {
		if utf8.RuneCountInString(d) > MaxDescription || !utf8.ValidString(d) || strings.ContainsRune(d, 0) {
			return v, ErrInvalidDescription
		}
		v.description = &d
	} else if p.Description != "" {
		return v, ErrInvalidDescription // only spaces
	}
	if m := bytes.TrimSpace(p.Metadata); len(m) > 0 && !bytes.Equal(m, []byte("null")) {
		if m[0] != '{' || !json.Valid(m) {
			return v, ErrInvalidMetadata
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, m); err != nil {
			return v, ErrInvalidMetadata
		}
		if buf.Len() > MaxMetadataBytes || bytes.Contains(buf.Bytes(), []byte(`\u0000`)) {
			return v, ErrInvalidMetadata
		}
		v.metadata = buf.Bytes()
	}
	if a := strings.ToUpper(strings.TrimSpace(p.AccountNumber)); a != "" {
		if len(a) > accountref.MaxLen {
			return v, ErrInvalidAccountNumber
		}
		v.account = a
	}
	if strings.TrimSpace(p.Phone) != "" {
		n, err := phone.Normalize(p.Phone)
		if err != nil {
			return v, ErrInvalidPhone
		}
		v.phone = n
	}
	v.expiresIn = p.ExpiresIn
	if v.expiresIn == 0 {
		v.expiresIn = DefaultExpiresIn
	}
	if v.expiresIn < MinExpiresIn || v.expiresIn > MaxExpiresIn {
		return v, ErrInvalidExpiry
	}
	v.hash = v.requestHash()
	return v, nil
}

// requestHash fingerprints what the app asked for (after normalisation), so
// a retry with the same key and the same request replays it.
func (v validated) requestHash() []byte {
	h := sha256.New()
	field := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	field(v.reference)
	field(strconv.FormatInt(v.amount, 10))
	if v.description != nil {
		field("d:" + *v.description)
	} else {
		field("")
	}
	field(string(v.metadata))
	field(v.account)
	field(v.phone)
	field(strconv.FormatInt(int64(v.expiresIn/time.Second), 10))
	return h.Sum(nil)
}

func printable(s string) bool {
	return utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}

// hostedToken is the pay-page token: 32 bytes from crypto/rand, lowercase
// unpadded base32 (52 characters).
func hostedToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Get returns an app's charge.
func (s *Service) Get(ctx context.Context, appID, id uuid.UUID) (Charge, error) {
	c, err := store.New(s.o.Pool).GetChargeForApp(ctx, store.GetChargeForAppParams{AppID: appID, ID: id})
	if db.IsNotFound(err) {
		return Charge{}, ErrNotFound
	}
	return c, err
}

// MaxListLimit is the most rows List returns (200 per page, plus one so the
// caller can tell whether there is a next page).
const MaxListLimit = 201

// List returns an app's charges, newest first (Limit+1 rows are fetched by
// callers that need a next cursor). Limit 0 means 50; above MaxListLimit is
// capped.
func (s *Service) List(ctx context.Context, appID uuid.UUID, p ListParams) ([]Charge, error) {
	if p.Status != nil && !p.Status.Valid() {
		return nil, ErrInvalidStatus
	}
	if (p.CursorAt == nil) != (p.CursorID == nil) {
		return nil, ErrInvalidCursor
	}
	switch {
	case p.Limit <= 0:
		p.Limit = 50
	case p.Limit > MaxListLimit:
		p.Limit = MaxListLimit
	}
	var ref *string
	if r := strings.TrimSpace(p.Reference); r != "" {
		ref = &r
	}
	var status store.NullChargeStatus
	if p.Status != nil {
		status = store.NullChargeStatus{ChargeStatus: *p.Status, Valid: true}
	}
	return store.New(s.o.Pool).ListChargesForApp(ctx, store.ListChargesForAppParams{
		AppID: appID, Status: status, Reference: ref, CursorAt: p.CursorAt, CursorID: p.CursorID, Lim: int32(p.Limit),
	})
}

// Cancel cancels a pending charge with no live prompt and emits
// charge.cancelled. A cancelled charge can still be paid by late money
// (rule 6). Cancelling an already cancelled charge is an idempotent no-op;
// anything else (paid, in review, expired, a prompt on the customer's
// phone) is ErrNotCancellable.
func (s *Service) Cancel(ctx context.Context, actor audit.Actor, appID, id uuid.UUID) (Cancelled, error) {
	if err := actor.Validate(); err != nil {
		return Cancelled{}, err
	}
	var out Cancelled
	err := db.WithTx(ctx, s.o.Pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		out = Cancelled{}
		c, err := q.LockCharge(ctx, store.LockChargeParams{AppID: appID, ID: id})
		if db.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch c.Status {
		case store.ChargeStatusCancelled:
			out = Cancelled{Charge: c, AlreadyCancelled: true}
			return nil
		case store.ChargeStatusPending:
		default:
			return ErrNotCancellable
		}
		// A prompt still on the customer's phone may yet be paid.
		if _, err := q.GetLiveSTKPushForCharge(ctx, c.ID); err == nil {
			return ErrNotCancellable
		} else if !db.IsNotFound(err) {
			return err
		}
		if out.Charge, err = q.CancelCharge(ctx, store.CancelChargeParams{AppID: appID, ID: id}); err != nil {
			return fmt.Errorf("charge: cancel: %w", err)
		}
		if err := s.o.Events.EmitCharge(ctx, q, appID, id, events.ChargeCancelled); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "charge.cancelled", EntityType: "charge", EntityID: id.String(), AppID: &appID,
			Details: map[string]any{"reference": c.Reference},
		})
	})
	return out, err
}

// Expire expires up to batch due pending charges (charge expiry job) and
// emits charge.expired for each. Returns how many expired.
func (s *Service) Expire(ctx context.Context, batch int) (int, error) {
	rows, err := ExpireDue(ctx, s.o.Pool, s.o.Events, batch)
	return len(rows), err
}

// ExpiryActor is recorded on every charge the expiry job expires.
var ExpiryActor = audit.System("expiry-job")

// ExpireDue is the one implementation of charge expiry (used by Expire and
// the worker's periodic job): in one transaction it expires up to batch
// pending charges whose expires_at has passed and emits charge.expired for
// each, in expiry order.
//
//   - Only pending charges expire, never needs_review (staff are checking a
//     payment). Late money still succeeds an expired charge (rule 6).
//   - Race-safe with payments: rows are claimed FOR NO KEY UPDATE SKIP
//     LOCKED, so a charge being paid right now is left for the next run.
//   - Audit: one row per app per batch ("charges.expired" with the count
//     and ids): expiry is routine, each charge already has its own event,
//     and the audit chain is serialised by a global lock.
func ExpireDue(ctx context.Context, pool db.Beginner, em *events.Emitter, batch int) ([]Charge, error) {
	if batch <= 0 || batch > 10_000 {
		batch = 500
	}
	var out []Charge
	err := db.WithTx(ctx, pool, ExpiryActor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		rows, err := q.ExpireDueCharges(ctx, int32(batch))
		if err != nil {
			return fmt.Errorf("charge: expire: %w", err)
		}
		// RETURNING order is unspecified; emit in expiry order so the event
		// feed reads naturally.
		slices.SortFunc(rows, func(a, b Charge) int {
			if c := a.ExpiresAt.Compare(b.ExpiresAt); c != 0 {
				return c
			}
			return cmp.Compare(a.ID.String(), b.ID.String())
		})
		byApp := map[uuid.UUID][]string{}
		var apps []uuid.UUID
		for _, r := range rows {
			if err := em.EmitCharge(ctx, q, r.AppID, r.ID, events.ChargeExpired); err != nil {
				return err
			}
			if _, ok := byApp[r.AppID]; !ok {
				apps = append(apps, r.AppID)
			}
			byApp[r.AppID] = append(byApp[r.AppID], r.ID.String())
		}
		for _, aid := range apps {
			ids := byApp[aid]
			if err := audit.Record(ctx, q, audit.Entry{
				Actor: ExpiryActor, Action: "charges.expired", EntityType: "charge", AppID: &aid,
				Details: map[string]any{"count": len(ids), "charge_ids": ids},
			}); err != nil {
				return err
			}
		}
		out = rows
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
