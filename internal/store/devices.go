package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/model"
)

// CreateDevice registers a delivery target.
func (s *Store) CreateDevice(d *model.Device) error {
	if d.ID == "" {
		d.ID = model.NewID("dev")
	}
	if d.AccountID == "" {
		d.AccountID = DefaultAccountID
	}
	if d.Platform == "" {
		d.Platform = model.PlatformOther
	}
	d.CreatedAt = time.Now().UTC()

	_, err := s.db.Exec(
		`INSERT INTO devices (id, account_id, name, transport, platform, config, created_at, disabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		d.ID, d.AccountID, d.Name, d.Transport, string(d.Platform), marshalMap(d.Config), d.CreatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("create device: %w", err)
	}
	return nil
}

const deviceCols = `id, account_id, name, transport, platform, config, created_at, last_seen, disabled`

func scanDevice(sc interface{ Scan(...any) error }) (*model.Device, error) {
	var (
		d        model.Device
		platform string
		cfg      string
		created  int64
		lastSeen sql.NullInt64
		disabled int
	)
	if err := sc.Scan(&d.ID, &d.AccountID, &d.Name, &d.Transport, &platform, &cfg, &created, &lastSeen, &disabled); err != nil {
		return nil, err
	}
	d.Platform = model.Platform(platform)
	d.Config = unmarshalMap(cfg)
	d.CreatedAt = fromUnix(created)
	d.LastSeen = fromNullUnix(lastSeen)
	d.Disabled = disabled == 1
	return &d, nil
}

// Device looks up one device by ID.
func (s *Store) Device(id string) (*model.Device, error) {
	row := s.db.QueryRow(`SELECT `+deviceCols+` FROM devices WHERE id = ?`, id)
	d, err := scanDevice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup device: %w", err)
	}
	return d, nil
}

// ListDevices returns devices for the default account. When enabledOnly is
// true, disabled devices are excluded — that is what the delivery path wants.
func (s *Store) ListDevices(enabledOnly bool) ([]*model.Device, error) {
	q := `SELECT ` + deviceCols + ` FROM devices WHERE account_id = ?`
	if enabledOnly {
		q += ` AND disabled = 0`
	}
	q += ` ORDER BY created_at ASC`

	rows, err := s.db.Query(q, DefaultAccountID)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()

	var out []*model.Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeviceByConfig finds a device by an exact value of one config key. Web Push
// uses it to detect a re-subscribe of an endpoint we already know.
func (s *Store) DeviceByConfig(transport, key, value string) (*model.Device, error) {
	rows, err := s.db.Query(
		`SELECT `+deviceCols+` FROM devices WHERE account_id = ? AND transport = ?`,
		DefaultAccountID, transport,
	)
	if err != nil {
		return nil, fmt.Errorf("scan devices: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		if d.Config[key] == value {
			return d, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return nil, ErrNotFound
}

// UpdateDeviceConfig replaces a device's transport config.
func (s *Store) UpdateDeviceConfig(id string, cfg map[string]string) error {
	_, err := s.db.Exec(`UPDATE devices SET config = ? WHERE id = ?`, marshalMap(cfg), id)
	if err != nil {
		return fmt.Errorf("update device config: %w", err)
	}
	return nil
}

// TouchDevice records that a device was successfully reached.
func (s *Store) TouchDevice(id string) error {
	_, err := s.db.Exec(`UPDATE devices SET last_seen = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

// SetDeviceDisabled enables or disables a device without deleting it.
func (s *Store) SetDeviceDisabled(id string, disabled bool) error {
	v := 0
	if disabled {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE devices SET disabled = ? WHERE id = ?`, v, id)
	if err != nil {
		return fmt.Errorf("set device disabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteDevice removes a device permanently.
func (s *Store) DeleteDevice(id string) error {
	res, err := s.db.Exec(`DELETE FROM devices WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete device: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
