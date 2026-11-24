package persistent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

// outletColumns reads an outlet with its merchant name and weekly hours.
const outletColumns = `
	o.id, o.merchant_id, m.name, o.pos_outlet_id, o.name, o.address, o.phone, o.base_lat, o.base_lng,
	o.location_mode, o.location_sharing, o.timezone, o.online_status, o.prep_time_minutes,
	o.pricing_valid, o.active, o.published, o.state_version, o.catalog_version,
	o.session_active, o.session_id, o.session_started_at, o.session_sharing, o.session_spot,
	o.cur_lat, o.cur_lng, o.cur_accuracy_m, o.cur_heading_deg, o.cur_moving, o.cur_recorded_at,
	o.pos_lat, o.pos_lng, o.pos_mode,
	EXISTS (SELECT 1 FROM merchant_gateways g WHERE g.merchant_id = o.merchant_id AND g.active),
	COALESCE((SELECT jsonb_agg(jsonb_build_object('dayOfWeek', h.day_of_week, 'opensAt', h.opens_at, 'closesAt', h.closes_at)
	                           ORDER BY h.day_of_week, h.opens_at)
	          FROM outlet_hours h WHERE h.outlet_id = o.id), '[]'::jsonb)`

const outletFrom = ` FROM outlets o JOIN merchants m ON m.id = o.merchant_id `

func scanOutlet(row pgx.Row, extra ...any) (*entity.Outlet, error) {
	var (
		o         entity.Outlet
		sessionID *string
		sharing   *string
		spotJSON  []byte
		hoursRaw  []byte
		posLat    *float64
		posLng    *float64
		posMode   *string
	)

	dest := []any{
		&o.ID, &o.MerchantID, &o.MerchantName, &o.POSOutletID, &o.Name, &o.Address, &o.Phone, &o.BaseLat, &o.BaseLng,
		&o.LocationMode, &o.LocationSharing, &o.Timezone, &o.OnlineStatus, &o.PrepTimeMinutes,
		&o.PricingValid, &o.Active, &o.Published, &o.StateVersion, &o.CatalogVersion,
		&o.Session.Active, &sessionID, &o.Session.StartedAt, &sharing, &spotJSON,
		&o.Current.Latitude, &o.Current.Longitude, &o.Current.AccuracyM, &o.Current.HeadingDeg, &o.Current.Moving, &o.Current.RecordedAt,
		&posLat, &posLng, &posMode,
		&o.OnlinePayment, &hoursRaw,
	}
	dest = append(dest, extra...)

	if err := row.Scan(dest...); err != nil {
		return nil, err
	}

	if sessionID != nil {
		o.Session.ID = *sessionID
	}

	if sharing != nil {
		o.Session.Sharing = *sharing
	}

	if len(spotJSON) > 0 {
		var spot entity.Spot
		if err := json.Unmarshal(spotJSON, &spot); err == nil {
			o.Session.Spot = &spot
		}
	}

	if err := json.Unmarshal(hoursRaw, &o.Hours); err != nil {
		return nil, fmt.Errorf("decode hours: %w", err)
	}

	if posLat != nil && posLng != nil && posMode != nil {
		o.Pos = &entity.Position{Latitude: *posLat, Longitude: *posLng, Mode: *posMode}
		if *posMode == entity.SharingLive {
			o.Pos.AccuracyM, o.Pos.HeadingDeg = o.Current.AccuracyM, o.Current.HeadingDeg
			o.Pos.Moving, o.Pos.RecordedAt = o.Current.Moving, o.Current.RecordedAt
		}
	}

	return &o, nil
}

// lockOutlet serializes every change to one outlet (events of one outlet can arrive
// on several workers) and returns its row, or nil when we do not know it yet.
func lockOutlet(ctx context.Context, tx txn, merchantID, posOutletID string) (*entity.Outlet, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "outlet:"+merchantID+":"+posOutletID); err != nil {
		return nil, err
	}

	o, err := scanOutlet(tx.QueryRow(ctx,
		`SELECT `+outletColumns+outletFrom+`WHERE o.merchant_id = $1 AND o.pos_outlet_id = $2 FOR UPDATE OF o`,
		merchantID, posOutletID))
	if err == pgx.ErrNoRows {
		return nil, nil //nolint:nilnil // absence is a normal answer here
	}

	return o, err
}

// writePosition derives where customers find the outlet and stores it with the search columns.
func writePosition(ctx context.Context, tx txn, id string) error {
	o, err := scanOutlet(tx.QueryRow(ctx, `SELECT `+outletColumns+outletFrom+`WHERE o.id = $1`, id))
	if err != nil {
		return err
	}

	p := o.DerivePosition()
	if p == nil {
		_, err = tx.Exec(ctx, `UPDATE outlets SET pos_lat = NULL, pos_lng = NULL, pos_mode = NULL, updated_at = now() WHERE id = $1`, id)

		return err
	}

	_, err = tx.Exec(ctx,
		`UPDATE outlets SET pos_lat = $2, pos_lng = $3, pos_mode = $4, updated_at = now() WHERE id = $1`,
		id, p.Latitude, p.Longitude, p.Mode)

	return err
}

// ApplyOutletState stores an outlet's profile, hours and settings if the POS version is newer.
// A first sight creates the outlet. eventAt orders publication changes.
func (s *Store) ApplyOutletState(ctx context.Context, merchantID string, st entity.POSOutlet, eventAt time.Time) error {
	return s.inTx(ctx, func(tx txn) error {
		cur, err := lockOutlet(ctx, tx, merchantID, st.OutletID)
		if err != nil {
			return err
		}

		var id string

		switch {
		case cur == nil:
			err = tx.QueryRow(ctx, `
				INSERT INTO outlets (merchant_id, pos_outlet_id, name, location_mode, location_sharing, timezone, online_status)
				VALUES ($1, $2, $3, 'FIXED', 'LIVE', 'Asia/Jakarta', 'AUTO') RETURNING id`,
				merchantID, st.OutletID, st.Name).Scan(&id)
			if err != nil {
				return err
			}
		case st.StateVersion <= cur.StateVersion:
			// Not newer: keep the state, but a newer event may still publish the outlet.
			return s.publishAt(ctx, tx, cur.ID, eventAt)
		default:
			id = cur.ID
		}

		if _, err := tx.Exec(ctx, `
			UPDATE outlets SET name = $2, address = $3, phone = $4, base_lat = $5, base_lng = $6,
			  location_mode = $7, location_sharing = $8, timezone = $9, online_status = $10,
			  prep_time_minutes = $11, pricing_valid = $12, active = $13, state_version = $14, updated_at = now()
			WHERE id = $1`,
			id, st.Name, st.Address, st.Phone, st.Latitude, st.Longitude,
			orDefault(st.LocationMode, entity.ModeFixed), orDefault(st.LocationSharing, entity.SharingLive),
			orDefault(st.Timezone, "Asia/Jakarta"), orDefault(st.OnlineStatus, entity.StatusAuto),
			max(st.PrepTimeMinutes, 1), st.PricingValid, st.Active, st.StateVersion); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `DELETE FROM outlet_hours WHERE outlet_id = $1`, id); err != nil {
			return err
		}

		for _, h := range st.Hours {
			if _, err := tx.Exec(ctx, `
				INSERT INTO outlet_hours (outlet_id, day_of_week, opens_at, closes_at) VALUES ($1, $2, $3, $4)
				ON CONFLICT DO NOTHING`, id, h.DayOfWeek, h.OpensAt, h.ClosesAt); err != nil {
				return err
			}
		}

		if err := s.publishAt(ctx, tx, id, eventAt); err != nil {
			return err
		}

		return writePosition(ctx, tx, id)
	})
}

// publishAt lists the outlet unless a newer publication change was already applied.
func (s *Store) publishAt(ctx context.Context, tx txn, id string, at time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE outlets SET published = true, published_event_at = $2, updated_at = now()
		WHERE id = $1 AND (published_event_at IS NULL OR published_event_at < $2)`, id, at)

	return err
}

// Unpublish removes an outlet from search (the merchant stopped publishing it).
func (s *Store) Unpublish(ctx context.Context, merchantID, posOutletID string, eventAt time.Time) error {
	return s.inTx(ctx, func(tx txn) error {
		cur, err := lockOutlet(ctx, tx, merchantID, posOutletID)
		if err != nil {
			return err
		}

		if cur == nil {
			return nil // Never listed; nothing to hide.
		}

		_, err = tx.Exec(ctx, `
			UPDATE outlets SET published = false, published_event_at = $2, updated_at = now()
			WHERE id = $1 AND (published_event_at IS NULL OR published_event_at < $2)`, cur.ID, eventAt)

		return err
	})
}

// SessionChange is a working day starting, ending or changing what it shares.
type SessionChange struct {
	Active    bool
	SessionID string
	StartedAt *time.Time
	Sharing   string
	Spot      *entity.Spot
}

// ApplySession records today's working day. Older changes than the last one are ignored.
func (s *Store) ApplySession(ctx context.Context, merchantID, posOutletID string, c SessionChange, at time.Time) error {
	return s.inTx(ctx, func(tx txn) error {
		cur, err := lockOutlet(ctx, tx, merchantID, posOutletID)
		if err != nil {
			return err
		}

		if cur == nil {
			return entity.ErrOutletUnknown
		}

		var spot []byte
		if c.Spot != nil {
			if spot, err = json.Marshal(c.Spot); err != nil {
				return err
			}
		}

		var sharing *string
		if c.Active {
			sharing = &c.Sharing
		}

		tag, err := tx.Exec(ctx, `
			UPDATE outlets SET session_active = $2, session_id = $3, session_started_at = $4,
			  session_sharing = $5, session_spot = $6, session_event_at = $7, updated_at = now()
			WHERE id = $1 AND (session_event_at IS NULL OR session_event_at < $7)`,
			cur.ID, c.Active, nullIfEmpty(c.SessionID), c.StartedAt, sharing, spot, at)
		if err != nil {
			return err
		}

		if tag.RowsAffected() == 0 {
			return nil // A newer change was applied already.
		}

		if !c.Active {
			if _, err := tx.Exec(ctx, `
				UPDATE outlets SET cur_lat = NULL, cur_lng = NULL, cur_accuracy_m = NULL, cur_heading_deg = NULL,
				  cur_moving = false, cur_recorded_at = NULL, cur_cleared_at = GREATEST(cur_cleared_at, $2)
				WHERE id = $1`, cur.ID, at); err != nil {
				return err
			}
		}

		return writePosition(ctx, tx, cur.ID)
	})
}

// LocationChange is a vendor's latest position.
type LocationChange struct {
	Latitude   float64
	Longitude  float64
	AccuracyM  *float64
	HeadingDeg *float64
	Moving     bool
	RecordedAt time.Time
}

// ApplyLocation stores the newest position. Older ones, and ones from before the last
// clearing, are ignored.
func (s *Store) ApplyLocation(ctx context.Context, merchantID, posOutletID string, l LocationChange) error {
	return s.inTx(ctx, func(tx txn) error {
		cur, err := lockOutlet(ctx, tx, merchantID, posOutletID)
		if err != nil {
			return err
		}

		if cur == nil {
			return entity.ErrOutletUnknown
		}

		tag, err := tx.Exec(ctx, `
			UPDATE outlets SET cur_lat = $2, cur_lng = $3, cur_accuracy_m = $4, cur_heading_deg = $5,
			  cur_moving = $6, cur_recorded_at = $7, updated_at = now()
			WHERE id = $1
			  AND (cur_recorded_at IS NULL OR cur_recorded_at < $7)
			  AND (cur_cleared_at IS NULL OR cur_cleared_at < $7)`,
			cur.ID, l.Latitude, l.Longitude, l.AccuracyM, l.HeadingDeg, l.Moving, l.RecordedAt)
		if err != nil {
			return err
		}

		if tag.RowsAffected() == 0 {
			return nil
		}

		return writePosition(ctx, tx, cur.ID)
	})
}

// ClearLocation forgets the vendor's position (day ended or sharing hidden).
func (s *Store) ClearLocation(ctx context.Context, merchantID, posOutletID string, at time.Time) error {
	return s.inTx(ctx, func(tx txn) error {
		cur, err := lockOutlet(ctx, tx, merchantID, posOutletID)
		if err != nil {
			return err
		}

		if cur == nil {
			return nil
		}

		if _, err := tx.Exec(ctx, `
			UPDATE outlets SET cur_lat = NULL, cur_lng = NULL, cur_accuracy_m = NULL, cur_heading_deg = NULL,
			  cur_moving = false, cur_recorded_at = NULL, cur_cleared_at = GREATEST(cur_cleared_at, $2), updated_at = now()
			WHERE id = $1`, cur.ID, at); err != nil {
			return err
		}

		return writePosition(ctx, tx, cur.ID)
	})
}

// ApplyCatalog replaces an outlet's menu if the snapshot is newer than the one we hold.
// It reports whether the menu changed.
func (s *Store) ApplyCatalog(ctx context.Context, merchantID, posOutletID string, version int, payload json.RawMessage, items []entity.MenuItem) (bool, error) {
	changed := false

	err := s.inTx(ctx, func(tx txn) error {
		cur, err := lockOutlet(ctx, tx, merchantID, posOutletID)
		if err != nil {
			return err
		}

		if cur == nil {
			return entity.ErrOutletUnknown
		}

		if version < cur.CatalogVersion || (version == cur.CatalogVersion && s.hasMenu(ctx, tx, cur.ID)) {
			return nil
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO outlet_menus (outlet_id, catalog_version, payload) VALUES ($1, $2, $3)
			ON CONFLICT (outlet_id) DO UPDATE SET catalog_version = $2, payload = $3, fetched_at = now()`,
			cur.ID, version, []byte(payload)); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `DELETE FROM menu_items WHERE outlet_id = $1`, cur.ID); err != nil {
			return err
		}

		for _, it := range items {
			if _, err := tx.Exec(ctx, `
				INSERT INTO menu_items (outlet_id, pos_product_id, name, category, price_minor, sold_out)
				VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
				cur.ID, it.POSProductID, it.Name, it.Category, it.PriceMinor, it.SoldOut); err != nil {
				return err
			}
		}

		changed = true

		_, err = tx.Exec(ctx, `UPDATE outlets SET catalog_version = $2, updated_at = now() WHERE id = $1`, cur.ID, version)

		return err
	})

	return changed, err
}

func (s *Store) hasMenu(ctx context.Context, tx txn, outletID string) bool {
	var one int

	return tx.QueryRow(ctx, `SELECT 1 FROM outlet_menus WHERE outlet_id = $1`, outletID).Scan(&one) == nil
}

// MenuAge reports when an outlet's menu was last fetched (nil when never).
func (s *Store) MenuAge(ctx context.Context, merchantID, posOutletID string) (*time.Time, error) {
	var at *time.Time

	err := s.pool.QueryRow(ctx, `
		SELECT mn.fetched_at FROM outlets o LEFT JOIN outlet_menus mn ON mn.outlet_id = o.id
		WHERE o.merchant_id = $1 AND o.pos_outlet_id = $2`, merchantID, posOutletID).Scan(&at)
	if err == pgx.ErrNoRows {
		return nil, nil //nolint:nilnil // unknown outlet has no menu
	}

	return at, err
}

// UnpublishMerchant hides every outlet of a merchant whose link was revoked at the POS.
func (s *Store) UnpublishMerchant(ctx context.Context, merchantID string) error {
	return s.inTx(ctx, func(tx txn) error {
		if _, err := tx.Exec(ctx, `UPDATE merchant_links SET status = 'revoked', updated_at = now() WHERE merchant_id = $1`, merchantID); err != nil {
			return err
		}

		_, err := tx.Exec(ctx, `UPDATE outlets SET published = false, updated_at = now() WHERE merchant_id = $1`, merchantID)

		return err
	})
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}

	return v
}

func nullIfEmpty(v string) *string {
	if v == "" {
		return nil
	}

	return &v
}

// UnpublishMissing hides outlets the POS no longer lists, unless a newer publication
// change than the listing was already applied.
func (s *Store) UnpublishMissing(ctx context.Context, merchantID string, keepPOSIDs []string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE outlets SET published = false, published_event_at = $3, updated_at = now()
		WHERE merchant_id = $1 AND published AND NOT (pos_outlet_id = ANY($2))
		  AND (published_event_at IS NULL OR published_event_at < $3)`, merchantID, keepPOSIDs, at)

	return err
}

// OutletByPOSID returns a merchant's outlet whether or not it is listed (for operators and tests).
func (s *Store) OutletByPOSID(ctx context.Context, merchantID, posOutletID string) (*entity.Outlet, error) {
	o, err := scanOutlet(s.pool.QueryRow(ctx,
		`SELECT `+outletColumns+outletFrom+`WHERE o.merchant_id = $1 AND o.pos_outlet_id = $2`, merchantID, posOutletID))
	if err != nil {
		return nil, notFound(err)
	}

	return o, nil
}
