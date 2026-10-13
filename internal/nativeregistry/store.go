// Package nativeregistry stores native session descriptions, never terminal or account data.
// matrix/hardened: one OS owner holds each explicit private registry root until SQLite is closed.
package nativeregistry

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const (
	DatabaseFilename = "registry.sqlite"
	OwnerFilename    = "owner.lock"
	schemaVersion    = 1
	maxLaunchBytes   = 64 * 1024
)

var (
	ErrOwned       = errors.New("native registry already has an owner")
	ErrNotFound    = errors.New("native registry session not found")
	ErrClosed      = errors.New("native registry is closed")
	ErrInvalidData = errors.New("invalid native registry data")
	ErrUnsafePath  = errors.New("unsafe native registry path")
)

type Record struct {
	ID        string                  `json:"id"`
	Name      string                  `json:"name"`
	Tool      string                  `json:"tool"`
	Launch    terminalhost.LaunchSpec `json:"launch"`
	CreatedAt time.Time               `json:"created_at"`
	UpdatedAt time.Time               `json:"updated_at"`
}

type Store struct {
	mu       sync.Mutex
	db       *sql.DB
	dir      string
	root     *os.File
	owner    *os.File
	data     *os.File
	closeErr error
}

// Open requires an absolute dedicated state directory under trusted existing
// ancestors. It creates only the final directory and its registry files. The
// caller must not use a project, account directory or another application's root.
// matrix/hardened: no fallback to HOME or a production registry is permitted.
func Open(dir string) (_ *Store, resultErr error) {
	if err := prepareDirectory(dir); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	defer func() {
		if resultErr != nil {
			_ = s.Close()
		}
	}()
	var err error
	s.owner, err = acquireOwner(filepath.Join(dir, OwnerFilename))
	if err != nil {
		return nil, err
	}
	s.root, err = os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("hold registry root: %w", err)
	}
	s.data, err = prepareData(filepath.Join(dir, DatabaseFilename))
	if err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if err := protectOptionalData(filepath.Join(dir, DatabaseFilename) + suffix); err != nil {
			return nil, err
		}
	}
	if err := s.verifyPaths(); err != nil {
		return nil, err
	}
	// matrix/hardened: URI-escape directory characters so '?' cannot redirect
	// SQLite to another pathname or be interpreted as connection options.
	dataPath := filepath.ToSlash(filepath.Join(dir, DatabaseFilename))
	if !strings.HasPrefix(dataPath, "/") {
		dataPath = "/" + dataPath
	}
	s.db, err = sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dataPath}).String())
	if err != nil {
		return nil, fmt.Errorf("open native registry: %w", err)
	}
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	if err := s.initialize(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) initialize() error {
	for _, statement := range []string{"PRAGMA busy_timeout=1000", "PRAGMA foreign_keys=ON", "PRAGMA trusted_schema=OFF"} {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("configure native registry: %w", err)
		}
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read native registry version: %w", err)
	}
	if version != 0 && version != schemaVersion {
		return fmt.Errorf("%w: unsupported schema version %d", ErrInvalidData, version)
	}
	if version == 0 {
		var tables int
		if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
			return err
		}
		if tables != 0 {
			return fmt.Errorf("%w: unversioned database already contains tables", ErrInvalidData)
		}
		if err := s.transaction(func(tx *sql.Tx) error {
			_, err := tx.Exec(`CREATE TABLE native_sessions (
				id TEXT PRIMARY KEY NOT NULL,
				name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 128),
				tool TEXT NOT NULL CHECK(length(tool) BETWEEN 1 AND 64),
				launch_json TEXT NOT NULL CHECK(length(launch_json) <= 65536),
				created_at INTEGER NOT NULL CHECK(created_at > 0),
				updated_at INTEGER NOT NULL CHECK(updated_at >= created_at)
			)`)
			if err != nil {
				return err
			}
			_, err = tx.Exec("PRAGMA user_version=1")
			return err
		}); err != nil {
			return fmt.Errorf("initialize native registry: %w", err)
		}
	}
	// matrix/hardened: rollback journals inherit the protected directory/DB;
	// persistent WAL/SHM files are not needed for a single exclusive manager.
	var journal string
	if err := s.db.QueryRow("PRAGMA journal_mode=DELETE").Scan(&journal); err != nil || journal != "delete" {
		return fmt.Errorf("configure native registry journal: %v", err)
	}
	if _, err := s.db.Exec("PRAGMA synchronous=FULL"); err != nil {
		return err
	}
	if err := s.checkSchema(); err != nil {
		return err
	}
	// Decode all rows before exposing the store; corrupted metadata cannot be
	// mistaken for launch instructions after a manager restart.
	_, err := s.list()
	return err
}

func (s *Store) checkSchema() error {
	var objects int
	if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type IN ('table','view','trigger') AND name NOT LIKE 'sqlite_%'").Scan(&objects); err != nil {
		return err
	}
	if objects != 1 {
		return fmt.Errorf("%w: unexpected schema objects", ErrInvalidData)
	}
	var kind string
	if err := s.db.QueryRow("SELECT type FROM sqlite_master WHERE name='native_sessions'").Scan(&kind); err != nil || kind != "table" {
		return fmt.Errorf("%w: session table missing", ErrInvalidData)
	}
	rows, err := s.db.Query("PRAGMA table_info(native_sessions)")
	if err != nil {
		return err
	}
	defer rows.Close()
	wantNames := []string{"id", "name", "tool", "launch_json", "created_at", "updated_at"}
	wantTypes := []string{"TEXT", "TEXT", "TEXT", "TEXT", "INTEGER", "INTEGER"}
	index := 0
	for rows.Next() {
		var column, notNull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&column, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		wantPK := 0
		if index == 0 {
			wantPK = 1
		}
		if index >= len(wantNames) || column != index || name != wantNames[index] || kind != wantTypes[index] || notNull != 1 || pk != wantPK || defaultValue != nil {
			return fmt.Errorf("%w: incompatible session table", ErrInvalidData)
		}
		index++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if index != len(wantNames) {
		return fmt.Errorf("%w: incompatible session columns", ErrInvalidData)
	}
	return nil
}

func (s *Store) transaction(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) checkOpen() error {
	if s.db == nil {
		return ErrClosed
	}
	return s.verifyPaths()
}

func (s *Store) verifyPaths() error {
	for path, file := range map[string]*os.File{s.dir: s.root, filepath.Join(s.dir, OwnerFilename): s.owner, filepath.Join(s.dir, DatabaseFilename): s.data} {
		if file == nil {
			return fmt.Errorf("%w: incomplete file ownership", ErrUnsafePath)
		}
		if err := verifyHeldPath(path, file); err != nil {
			return err
		}
	}
	return nil
}

// Close never removes the directory, lock file, metadata or any session data.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// matrix/hardened: release the database first, then held data/root handles,
	// and finally the owner lock so a new owner cannot overlap SQLite access.
	if s.db != nil {
		s.closeErr = errors.Join(s.closeErr, s.db.Close())
		s.db = nil
	}
	if s.data != nil {
		s.closeErr = errors.Join(s.closeErr, s.data.Close())
		s.data = nil
	}
	if s.root != nil {
		s.closeErr = errors.Join(s.closeErr, s.root.Close())
		s.root = nil
	}
	if s.owner != nil {
		s.closeErr = errors.Join(s.closeErr, releaseOwner(s.owner))
		s.owner = nil
	}
	return s.closeErr
}

func (s *Store) List() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	return s.list()
}

func (s *Store) list() ([]Record, error) {
	rows, err := s.db.Query("SELECT id,name,tool,launch_json,created_at,updated_at FROM native_sessions ORDER BY created_at,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]Record, 0)
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) Get(id string) (Record, error) {
	if err := ValidateID(id); err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return Record{}, err
	}
	return scanRecord(s.db.QueryRow("SELECT id,name,tool,launch_json,created_at,updated_at FROM native_sessions WHERE id=?", id))
}

func (s *Store) Create(name, tool string, launch terminalhost.LaunchSpec) (Record, error) {
	if err := ValidateName(name); err != nil {
		return Record{}, err
	}
	if err := validateTool(tool); err != nil {
		return Record{}, err
	}
	data, err := encodeLaunch(launch)
	if err != nil {
		return Record{}, err
	}
	var cloned terminalhost.LaunchSpec
	if err := json.Unmarshal(data, &cloned); err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return Record{}, err
	}
	now := time.Now().UTC()
	record := Record{ID: uuid.NewString(), Name: name, Tool: tool, Launch: cloned, CreatedAt: now, UpdatedAt: now}
	err = s.transaction(func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO native_sessions(id,name,tool,launch_json,created_at,updated_at) VALUES(?,?,?,?,?,?)", record.ID, name, tool, string(data), now.UnixNano(), now.UnixNano())
		return err
	})
	if err != nil {
		return Record{}, err
	}
	return record, nil
}

func (s *Store) Rename(id, name string) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return err
	}
	return s.transaction(func(tx *sql.Tx) error {
		result, err := tx.Exec("UPDATE native_sessions SET name=?,updated_at=max(updated_at,?) WHERE id=?", name, time.Now().UTC().UnixNano(), id)
		if err != nil {
			return err
		}
		return found(result)
	})
}

// Delete removes only a description; the fleet must explicitly stop a runtime
// first. This method does not traverse or remove the launch working directory.
func (s *Store) Delete(id string) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return err
	}
	return s.transaction(func(tx *sql.Tx) error {
		result, err := tx.Exec("DELETE FROM native_sessions WHERE id=?", id)
		if err != nil {
			return err
		}
		return found(result)
	})
}

func found(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrNotFound
	}
	return nil
}

type scanner interface{ Scan(...any) error }

func scanRecord(row scanner) (Record, error) {
	var record Record
	var raw string
	var created, updated int64
	if err := row.Scan(&record.ID, &record.Name, &record.Tool, &raw, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, ErrNotFound
		}
		return Record{}, err
	}
	if err := ValidateID(record.ID); err != nil {
		return Record{}, err
	}
	if err := ValidateName(record.Name); err != nil {
		return Record{}, err
	}
	if err := validateTool(record.Tool); err != nil {
		return Record{}, err
	}
	if len(raw) > maxLaunchBytes || !utf8.ValidString(raw) {
		return Record{}, fmt.Errorf("%w: oversized or invalid UTF-8 launch description", ErrInvalidData)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record.Launch); err != nil {
		return Record{}, fmt.Errorf("%w: launch description: %v", ErrInvalidData, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Record{}, fmt.Errorf("%w: trailing launch description data", ErrInvalidData)
	}
	if _, err := encodeLaunch(record.Launch); err != nil {
		return Record{}, err
	}
	if created <= 0 || updated < created {
		return Record{}, fmt.Errorf("%w: invalid description timestamps", ErrInvalidData)
	}
	record.CreatedAt = time.Unix(0, created).UTC()
	record.UpdatedAt = time.Unix(0, updated).UTC()
	return record, nil
}

func ValidateID(id string) error {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id || parsed.Version() != 4 || parsed.Variant() != uuid.RFC4122 {
		return fmt.Errorf("%w: require a canonical version-4 session UUID", ErrInvalidData)
	}
	return nil
}

func ValidateName(name string) error {
	if !utf8.ValidString(name) || len(name) > 512 || utf8.RuneCountInString(name) > 128 || strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: name must contain 1-128 Unicode characters", ErrInvalidData)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: name contains a control character", ErrInvalidData)
		}
	}
	return nil
}

func validateTool(tool string) error {
	if len(tool) == 0 || len(tool) > 64 {
		return fmt.Errorf("%w: tool must contain 1-64 ASCII characters", ErrInvalidData)
	}
	for _, r := range tool {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return fmt.Errorf("%w: invalid tool identifier", ErrInvalidData)
	}
	return nil
}

func encodeLaunch(launch terminalhost.LaunchSpec) ([]byte, error) {
	if err := terminalhost.ValidateLaunchSpec(launch); err != nil {
		return nil, fmt.Errorf("%w: launch description: %v", ErrInvalidData, err)
	}
	data, err := json.Marshal(launch)
	if err != nil {
		return nil, err
	}
	if len(data) > maxLaunchBytes {
		return nil, fmt.Errorf("%w: oversized launch description", ErrInvalidData)
	}
	return data, nil
}
