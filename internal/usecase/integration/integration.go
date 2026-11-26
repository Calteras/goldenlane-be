// Package integration keeps the platform's copy of each merchant's outlets, hours,
// positions and menus in step with the POS: it verifies and stores the events the POS
// sends, applies them in order, and reconciles against the POS to heal any gap.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/repository/persistent"
	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/pos"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
	"github.com/Calteras/streetfood-customer-api/pkg/secretbox"
	"github.com/Calteras/streetfood-customer-api/pkg/signing"
)

const (
	_maxEventsPerBatch = 100
	_claimBatch        = 20
	_lease             = 60 * time.Second
	_giveUpAfter       = 12
	_menuMaxAge        = 6 * time.Hour
	_maxIDLength       = 100
)

var _backoff = []time.Duration{5 * time.Second, 15 * time.Second, time.Minute, 5 * time.Minute}

// Store is what the use case needs from storage.
type Store interface {
	LinkByKeyID(ctx context.Context, keyID string) (entity.Link, error)
	LinkByMerchant(ctx context.Context, merchantID string) (entity.Link, error)
	ActiveLinks(ctx context.Context) ([]entity.Link, error)
	InsertInbox(ctx context.Context, keyID string, events []entity.InboxEvent) (int, error)
	ClaimInbox(ctx context.Context, limit int, lease time.Duration) ([]entity.InboxRow, string, error)
	FinishInbox(ctx context.Context, id, token string) error
	RetryInbox(ctx context.Context, id, token, message string, delay time.Duration, giveUp bool) error
	ApplyOutletState(ctx context.Context, merchantID string, st entity.POSOutlet, eventAt time.Time) error
	Unpublish(ctx context.Context, merchantID, posOutletID string, eventAt time.Time) error
	UnpublishMissing(ctx context.Context, merchantID string, keepPOSIDs []string, at time.Time) error
	ApplySession(ctx context.Context, merchantID, posOutletID string, c persistent.SessionChange, at time.Time) error
	ApplyLocation(ctx context.Context, merchantID, posOutletID string, l persistent.LocationChange) error
	ClearLocation(ctx context.Context, merchantID, posOutletID string, at time.Time) error
	ApplyCatalog(ctx context.Context, merchantID, posOutletID string, version int, payload json.RawMessage, items []entity.MenuItem) (bool, error)
	MenuAge(ctx context.Context, merchantID, posOutletID string) (*time.Time, error)
	UnpublishMerchant(ctx context.Context, merchantID string) error
	PendingFees(ctx context.Context, merchantID string) (entity.FeeReservation, error)
	ReserveFees(ctx context.Context, merchantID, ref string) (entity.FeeReservation, error)
	SettleFees(ctx context.Context, merchantID, ref, settlementRef string) (int, int64, error)
}

// POS reads a merchant's POS.
type POS interface {
	Outlets(ctx context.Context, cred pos.Credentials) ([]entity.POSOutlet, time.Time, error)
	Snapshot(ctx context.Context, cred pos.Credentials, posOutletID string) (*entity.POSSnapshot, error)
}

// OrderMirror applies order status changes the POS reports.
type OrderMirror interface {
	ApplyEvent(ctx context.Context, version int, data json.RawMessage) error
}

// UseCase is the integration service.
type UseCase struct {
	store  Store
	pos    POS
	box    *secretbox.Box
	log    logger.Interface
	now    func() time.Time
	orders OrderMirror
}

// New builds the use case.
func New(store Store, posClient POS, box *secretbox.Box, log logger.Interface) *UseCase {
	return &UseCase{store: store, pos: posClient, box: box, log: log, now: time.Now}
}

// SetOrderMirror connects order status events to the ordering service. Without one they
// are stored and ignored.
func (u *UseCase) SetOrderMirror(m OrderMirror) { u.orders = m }

// ---------------------------------------------------------------- receiving

type wireEvent struct {
	EventID    string          `json:"eventId"`
	Type       string          `json:"type"`
	OccurredAt time.Time       `json:"occurredAt"`
	OutletID   *string         `json:"outletId"`
	Version    int             `json:"version"`
	Data       json.RawMessage `json:"data"`
}

// Receive authenticates a batch of POS events and stores it. It does not apply them:
// the POS gets its answer fast and a crash loses nothing. Every failure to
// authenticate is the same ErrUnauthorized, so callers cannot probe for key ids.
func (u *UseCase) Receive(ctx context.Context, keyID, timestamp, signature, method, path string, body []byte) error {
	if _, err := u.authenticate(ctx, keyID, timestamp, signature, method, path, body); err != nil {
		return err
	}

	var batch struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(body, &batch); err != nil || len(batch.Events) == 0 || len(batch.Events) > _maxEventsPerBatch {
		return fmt.Errorf("%w: expected 1 to %d events", entity.ErrBadRequest, _maxEventsPerBatch)
	}

	events := make([]entity.InboxEvent, 0, len(batch.Events))

	for _, raw := range batch.Events {
		var e wireEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("%w: event is not valid", entity.ErrBadRequest)
		}

		if e.EventID == "" || len(e.EventID) > _maxIDLength || e.Type == "" || len(e.Type) > _maxIDLength || e.OccurredAt.IsZero() {
			return fmt.Errorf("%w: event needs eventId, type and occurredAt", entity.ErrBadRequest)
		}

		events = append(events, entity.InboxEvent{
			EventID: e.EventID, Type: e.Type, OutletID: e.OutletID, Version: e.Version,
			OccurredAt: e.OccurredAt, Payload: raw,
		})
	}

	if _, err := u.store.InsertInbox(ctx, keyID, events); err != nil {
		return fmt.Errorf("store events: %w", err)
	}

	return nil
}

// authenticate proves a request was signed by the merchant's POS and returns its link.
//
// The caller always gets the same ErrUnauthorized, so nobody can probe which key ids exist. The
// real reason is logged here (never the secret or the signature), because "401" alone cannot tell
// an unregistered key from a changed secret or a clock that is off. A storage failure is not a
// credential problem: it is returned as such, so the POS sees a server error and retries.
func (u *UseCase) authenticate(ctx context.Context, keyID, timestamp, signature, method, path string, body []byte) (entity.Link, error) {
	link, err := u.store.LinkByKeyID(ctx, keyID)
	if err != nil {
		if !errors.Is(err, entity.ErrNotFound) {
			return entity.Link{}, fmt.Errorf("look up key: %w", err)
		}

		u.log.Warn("pos request refused: no active link for this key id (not registered, replaced by a new secret, or the merchant is suspended)",
			"keyId", keyID, "path", path)

		return entity.Link{}, entity.ErrUnauthorized
	}

	secret, err := u.box.Open(link.SecretEnc, link.MerchantID)
	if err != nil {
		u.log.Error("pos request refused: the stored secret cannot be decrypted (was ENCRYPTION_KEY changed?)",
			"keyId", keyID, "merchant", link.MerchantID)

		return entity.Link{}, entity.ErrUnauthorized
	}

	if len(body) > signing.MaxBodyBytes {
		return entity.Link{}, fmt.Errorf("%w: body too large", entity.ErrBadRequest)
	}

	if err := signing.Verify(secret, timestamp, signature, method, path, body, u.now()); err != nil {
		reason := "the signature does not match: the POS holds a different secret than the platform"
		if errors.Is(err, signing.ErrTimestampSkew) {
			reason = "the timestamp is more than 5 minutes from this server's clock"
		}

		u.log.Warn("pos request refused: "+reason, "keyId", keyID, "merchant", link.MerchantID, "path", path)

		return entity.Link{}, entity.ErrUnauthorized
	}

	return link, nil
}

// PendingFees tells the POS what its merchant owes now, for the owner to see. Nothing is reserved.
func (u *UseCase) PendingFees(ctx context.Context, keyID, timestamp, signature, method, path string, body []byte) (entity.FeeReservation, error) {
	link, err := u.authenticate(ctx, keyID, timestamp, signature, method, path, body)
	if err != nil {
		return entity.FeeReservation{}, err
	}

	return u.store.PendingFees(ctx, link.MerchantID)
}

// ReserveFees sets aside what the merchant owes for one billing period and tells the POS how much
// to add to the subscription charge. The same period always gets the same answer.
func (u *UseCase) ReserveFees(ctx context.Context, keyID, timestamp, signature, method, path string, body []byte) (entity.FeeReservation, error) {
	link, err := u.authenticate(ctx, keyID, timestamp, signature, method, path, body)
	if err != nil {
		return entity.FeeReservation{}, err
	}

	var req struct {
		BillingRef string `json:"billingRef"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.BillingRef == "" || len(req.BillingRef) > 200 {
		return entity.FeeReservation{}, fmt.Errorf("%w: billingRef is required", entity.ErrBadRequest)
	}

	return u.store.ReserveFees(ctx, link.MerchantID, req.BillingRef)
}

// --------------------------------------------------------------- processing

// Process applies one batch of stored events. more tells the poller to run again at once.
func (u *UseCase) Process(ctx context.Context) (bool, error) {
	rows, token, err := u.store.ClaimInbox(ctx, _claimBatch, _lease)
	if err != nil {
		return false, err
	}

	for _, row := range rows {
		if err := u.apply(ctx, row); err != nil {
			delay := _backoff[min(row.Attempts-1, len(_backoff)-1)]
			giveUp := row.Attempts >= _giveUpAfter

			u.log.Warn("pos event failed", "type", row.Type, "attempt", row.Attempts, "giveUp", giveUp, "error", err.Error())

			if rerr := u.store.RetryInbox(ctx, row.ID, token, err.Error(), delay, giveUp); rerr != nil {
				return false, rerr
			}

			continue
		}

		if err := u.store.FinishInbox(ctx, row.ID, token); err != nil {
			return false, err
		}
	}

	return len(rows) == _claimBatch, nil
}

func (u *UseCase) apply(ctx context.Context, row entity.InboxRow) error {
	link, err := u.store.LinkByKeyID(ctx, row.KeyID)
	if errors.Is(err, entity.ErrNotFound) {
		// Suspended or revoked since the event arrived: nothing to apply to.
		return nil
	}

	if err != nil {
		return err
	}

	var e wireEvent
	if err := json.Unmarshal(row.Payload, &e); err != nil {
		return nil //nolint:nilerr // a stored event we cannot read will never become readable
	}

	outletID := ""
	if row.OutletID != nil {
		outletID = *row.OutletID
	}

	switch row.Type {
	case "outlet.updated":
		var d struct {
			Outlet entity.POSOutlet `json:"outlet"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil || d.Outlet.OutletID == "" {
			return nil //nolint:nilerr // malformed, drop
		}

		return u.store.ApplyOutletState(ctx, link.MerchantID, d.Outlet, row.OccurredAt)
	case "outlet.unpublished":
		return u.store.Unpublish(ctx, link.MerchantID, outletID, row.OccurredAt)
	case "catalog.changed":
		return u.refreshCatalog(ctx, link, outletID)
	case "outlet.session.changed":
		var d struct {
			Active    bool           `json:"active"`
			SessionID string         `json:"sessionId"`
			StartedAt *time.Time     `json:"startedAt"`
			Sharing   string         `json:"sharing"`
			Spot      *entity.Spot   `json:"spot"`
			Anchor    *entity.Anchor `json:"anchor"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return nil //nolint:nilerr // malformed, drop
		}

		return u.store.ApplySession(ctx, link.MerchantID, outletID, persistent.SessionChange{
			Active: d.Active, SessionID: d.SessionID, StartedAt: d.StartedAt, Sharing: d.Sharing, Spot: entity.Place(d.Spot, d.Anchor),
		}, row.OccurredAt)
	case "outlet.location.updated":
		var d entity.POSLocation
		if err := json.Unmarshal(e.Data, &d); err != nil || d.RecordedAt == nil {
			return nil //nolint:nilerr // malformed, drop
		}

		// A SPOT event repeats the chosen place; the place itself arrives with the session.
		if d.Mode != entity.SharingLive {
			return nil
		}

		return u.store.ApplyLocation(ctx, link.MerchantID, outletID, persistent.LocationChange{
			Latitude: d.Latitude, Longitude: d.Longitude, AccuracyM: d.AccuracyM,
			HeadingDeg: d.HeadingDeg, Moving: d.Moving, RecordedAt: *d.RecordedAt,
		})
	case "outlet.location.cleared":
		return u.store.ClearLocation(ctx, link.MerchantID, outletID, row.OccurredAt)
	case "billing.fees.settled":
		// The merchant's subscription payment that included the reserved fees succeeded.
		var d struct {
			BillingRef    string `json:"billingRef"`
			InvoiceNumber string `json:"invoiceNumber"`
			AmountMinor   int64  `json:"amountMinor"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil || d.BillingRef == "" {
			return nil //nolint:nilerr // malformed, drop
		}

		n, total, err := u.store.SettleFees(ctx, link.MerchantID, d.BillingRef, "invoice:"+d.InvoiceNumber)
		if err != nil {
			return err
		}

		if total != d.AmountMinor {
			// The money was taken either way; the difference is for a person to look at.
			u.log.Warn("settled fees differ from what the POS charged", "merchant", link.MerchantID,
				"ref", d.BillingRef, "entries", n, "ledger", total, "charged", d.AmountMinor)
		}

		return nil
	case "link.revoked":
		return u.store.UnpublishMerchant(ctx, link.MerchantID)
	case "order.status.changed":
		if u.orders == nil {
			return nil
		}

		return u.orders.ApplyEvent(ctx, row.Version, e.Data)
	default:
		// Unknown types are ignored so the POS can add events without breaking us.
		return nil
	}
}

// --------------------------------------------------------------- reconcile

func (u *UseCase) credentials(link entity.Link) (pos.Credentials, error) {
	secret, err := u.box.Open(link.SecretEnc, link.MerchantID)
	if err != nil {
		return pos.Credentials{}, fmt.Errorf("merchant secret unreadable: %w", err)
	}

	return pos.Credentials{BaseURL: link.BaseURL, KeyID: link.KeyID, Secret: secret}, nil
}

// refreshCatalog fetches the outlet's snapshot (events say what changed, the snapshot
// says what it is) and applies its state, today's position and its menu.
func (u *UseCase) refreshCatalog(ctx context.Context, link entity.Link, posOutletID string) error {
	cred, err := u.credentials(link)
	if err != nil {
		return err
	}

	snap, err := u.pos.Snapshot(ctx, cred, posOutletID)
	if err != nil {
		return err
	}

	if err := u.applyOutlet(ctx, link.MerchantID, snap.Outlet, snap.GeneratedAt); err != nil {
		return err
	}

	version, items, err := entity.ParseCatalog(snap.Catalog)
	if err != nil {
		return fmt.Errorf("catalog is not valid: %w", err)
	}

	_, err = u.store.ApplyCatalog(ctx, link.MerchantID, posOutletID, version, snap.Catalog, items)

	return err
}

// applyOutlet stores an outlet object from a list or snapshot: its state, then the
// working day and position it reports, all ordered by the POS clock (at).
func (u *UseCase) applyOutlet(ctx context.Context, merchantID string, o entity.POSOutlet, at time.Time) error {
	if err := u.store.ApplyOutletState(ctx, merchantID, o, at); err != nil {
		return err
	}

	change := persistent.SessionChange{Sharing: entity.SharingLive}
	if s := o.Session; s != nil {
		change = persistent.SessionChange{Active: s.Active, SessionID: s.SessionID, StartedAt: s.StartedAt, Sharing: s.Sharing, Spot: entity.Place(s.Spot, s.Anchor)}
	}

	if err := u.store.ApplySession(ctx, merchantID, o.OutletID, change, at); err != nil {
		return err
	}

	switch l := o.Location; {
	case l != nil && l.Mode == entity.SharingLive && l.RecordedAt != nil:
		return u.store.ApplyLocation(ctx, merchantID, o.OutletID, persistent.LocationChange{
			Latitude: l.Latitude, Longitude: l.Longitude, AccuracyM: l.AccuracyM,
			HeadingDeg: l.HeadingDeg, Moving: l.Moving, RecordedAt: *l.RecordedAt,
		})
	case change.Active:
		return nil // At a spot, or sharing nothing: the session already says where.
	default:
		return u.store.ClearLocation(ctx, merchantID, o.OutletID, at)
	}
}

// ReconcileLink re-reads one merchant's published outlets from the POS. It creates
// outlets we never heard about, repairs any drift, and fetches menus that are missing or old.
func (u *UseCase) ReconcileLink(ctx context.Context, link entity.Link) error {
	cred, err := u.credentials(link)
	if err != nil {
		return err
	}

	outlets, at, err := u.pos.Outlets(ctx, cred)
	if err != nil {
		return err
	}

	keep := make([]string, 0, len(outlets))

	for _, o := range outlets {
		keep = append(keep, o.OutletID)

		if err := u.applyOutlet(ctx, link.MerchantID, o, at); err != nil {
			return fmt.Errorf("outlet %s: %w", o.OutletID, err)
		}

		fetched, err := u.store.MenuAge(ctx, link.MerchantID, o.OutletID)
		if err != nil {
			return err
		}

		if fetched == nil || u.now().Sub(*fetched) > _menuMaxAge {
			if err := u.refreshCatalog(ctx, link, o.OutletID); err != nil {
				return fmt.Errorf("menu of outlet %s: %w", o.OutletID, err)
			}
		}
	}

	return u.store.UnpublishMissing(ctx, link.MerchantID, keep, at)
}

// Reconcile runs ReconcileLink for every active merchant; one failing merchant does not
// stop the others.
func (u *UseCase) Reconcile(ctx context.Context) (bool, error) {
	links, err := u.store.ActiveLinks(ctx)
	if err != nil {
		return false, err
	}

	for _, link := range links {
		if err := u.ReconcileLink(ctx, link); err != nil {
			u.log.Warn("reconcile failed", "merchant", link.MerchantID, "error", strings.TrimSpace(err.Error()))
		}
	}

	return false, nil
}
