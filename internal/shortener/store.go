// Package shortener holds the link store and the HTTP handlers.
package shortener

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Base62 without look-alike characters: no 0/O, 1/l/I. A code read aloud or
// copied off a screen survives the trip.
const alphabet = "23456789abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ"

var (
	ErrNotFound   = errors.New("no such link")
	ErrCodeTaken  = errors.New("that code is already in use")
	ErrBadURL     = errors.New("url must be http or https and have a host")
	ErrBadCode    = errors.New("a custom code must be 3-32 characters of letters, digits, - or _")
	ErrExpired    = errors.New("this link has expired")
	codePattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{3,32}$`)
	reservedCodes = map[string]bool{"api": true, "health": true, "static": true, "admin": true, "stats": true}
)

// Link is one shortened URL.
type Link struct {
	Code      string     `json:"code"`
	Target    string     `json:"target"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Visits    int64      `json:"visits"`
	LastVisit *time.Time `json:"last_visit,omitempty"`
}

// Store persists links. SQLite via database/sql, with the visit counter updated
// in the same statement that reads the row, so concurrent hits cannot lose counts.
type Store struct {
	db *sql.DB
	mu sync.Mutex // guards code generation retries
}

func NewStore(db *sql.DB) (*Store, error) {
	schema := `
	CREATE TABLE IF NOT EXISTS links (
		code       TEXT PRIMARY KEY,
		target     TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL,
		expires_at TIMESTAMP,
		visits     INTEGER NOT NULL DEFAULT 0,
		last_visit TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS links_created_idx ON links(created_at);`
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	return &Store{db: db}, nil
}

// NormaliseURL validates and tidies a target URL.
func NormaliseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrBadURL
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", ErrBadURL
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", ErrBadURL
	}
	if parsed.Host == "" || !strings.Contains(parsed.Host, ".") {
		return "", ErrBadURL
	}
	parsed.Host = strings.ToLower(parsed.Host)
	return parsed.String(), nil
}

// NewCode returns a random code of the given length.
func NewCode(length int) (string, error) {
	out := make([]byte, length)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max) // crypto/rand: codes should not be guessable
		if err != nil {
			return "", err
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out), nil
}

// Create stores a link. An empty code generates one; a custom code is validated
// and must not collide with an existing link or a reserved path.
func (s *Store) Create(target, code string, ttl time.Duration) (*Link, error) {
	normalised, err := NormaliseURL(target)
	if err != nil {
		return nil, err
	}
	if code != "" {
		if !codePattern.MatchString(code) || reservedCodes[strings.ToLower(code)] {
			return nil, ErrBadCode
		}
	}

	var expires *time.Time
	if ttl > 0 {
		t := time.Now().UTC().Add(ttl)
		expires = &t
	}
	link := &Link{Target: normalised, CreatedAt: time.Now().UTC(), ExpiresAt: expires}

	s.mu.Lock()
	defer s.mu.Unlock()
	if code != "" {
		if err := s.insert(code, link); err != nil {
			return nil, err
		}
		link.Code = code
		return link, nil
	}
	// Generated codes: grow the length if the space is busy rather than looping forever.
	for attempt := 0; attempt < 12; attempt++ {
		generated, err := NewCode(6 + attempt/4)
		if err != nil {
			return nil, err
		}
		err = s.insert(generated, link)
		if errors.Is(err, ErrCodeTaken) {
			continue
		}
		if err != nil {
			return nil, err
		}
		link.Code = generated
		return link, nil
	}
	return nil, errors.New("could not find a free code")
}

func (s *Store) insert(code string, link *Link) error {
	_, err := s.db.Exec(
		`INSERT INTO links (code, target, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		code, link.Target, link.CreatedAt, link.ExpiresAt)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrCodeTaken
	}
	return err
}

// Resolve returns the target for a code and counts the visit in one statement.
func (s *Store) Resolve(code string) (*Link, error) {
	link, err := s.Get(code)
	if err != nil {
		return nil, err
	}
	if link.ExpiresAt != nil && link.ExpiresAt.Before(time.Now().UTC()) {
		return nil, ErrExpired
	}
	if _, err := s.db.Exec(
		`UPDATE links SET visits = visits + 1, last_visit = ? WHERE code = ?`,
		time.Now().UTC(), code); err != nil {
		return nil, err
	}
	link.Visits++
	return link, nil
}

// Get reads a link without counting a visit.
func (s *Store) Get(code string) (*Link, error) {
	row := s.db.QueryRow(
		`SELECT code, target, created_at, expires_at, visits, last_visit FROM links WHERE code = ?`, code)
	link := &Link{}
	err := row.Scan(&link.Code, &link.Target, &link.CreatedAt, &link.ExpiresAt, &link.Visits, &link.LastVisit)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return link, err
}

// List returns the most recent links.
func (s *Store) List(limit int) ([]*Link, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT code, target, created_at, expires_at, visits, last_visit
		 FROM links ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var links []*Link
	for rows.Next() {
		link := &Link{}
		if err := rows.Scan(&link.Code, &link.Target, &link.CreatedAt, &link.ExpiresAt,
			&link.Visits, &link.LastVisit); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// Delete removes a link.
func (s *Store) Delete(code string) error {
	result, err := s.db.Exec(`DELETE FROM links WHERE code = ?`, code)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Stats summarises the store.
func (s *Store) Stats() (map[string]any, error) {
	var total, visits, expired int64
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(visits), 0),
		COALESCE(SUM(CASE WHEN expires_at IS NOT NULL AND expires_at < ? THEN 1 ELSE 0 END), 0)
		FROM links`, time.Now().UTC()).Scan(&total, &visits, &expired)
	if err != nil {
		return nil, err
	}
	return map[string]any{"links": total, "visits": visits, "expired": expired}, nil
}

// PurgeExpired deletes links whose time has passed; run it from a ticker.
func (s *Store) PurgeExpired() (int64, error) {
	result, err := s.db.Exec(`DELETE FROM links WHERE expires_at IS NOT NULL AND expires_at < ?`,
		time.Now().UTC())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
