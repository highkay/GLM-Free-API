// Account-token pool: registered Z.AI account bearer tokens, persisted in
// their own SQLite file (accounts.sqlite) so POST /sqlite device-token
// hot-swaps never wipe them.
//
// Design notes:
//   - Selection is "least-failed, then least-recently-used" — a token that
//     just 401'd is deprioritised until others have had a turn, and a token
//     that keeps failing past the disable threshold is skipped entirely.
//   - The pool is mutated through the /v1/tokens management API at runtime
//     (add/list/delete), so the holder is a plain RWMutex-guarded handle —
//     the hot-swap machinery in db.go is for the device-token file, whose
//     whole file is replaced at once; account tokens are edited row by row.
//
// Schema:
//
//	CREATE TABLE account_tokens (
//	    id           INTEGER PRIMARY KEY AUTOINCREMENT,
//	    token        TEXT NOT NULL UNIQUE,
//	    label        TEXT NOT NULL DEFAULT '',
//	    created_at   INTEGER NOT NULL,
//	    last_used_at INTEGER NOT NULL DEFAULT 0,
//	    fail_count   INTEGER NOT NULL DEFAULT 0,
//	    disabled     INTEGER NOT NULL DEFAULT 0
//	);
package zbridge

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// accountTokenFailDisableThreshold is the consecutive-failure count at which
// a token is auto-disabled (skipped by acquire) until someone re-enables or
// deletes it via the management API.
const accountTokenFailDisableThreshold = 5

var (
	accountDBMu   sync.RWMutex
	accountDB     *sql.DB
	accountDBPath string
)

// ErrAccountTokenStoreUnavailable is returned when the account-token store
// has not been initialised (initAccountTokenDB failed or was never called).
var ErrAccountTokenStoreUnavailable = errors.New("account token store unavailable")

// initAccountTokenDB opens (creating if absent) the account-token SQLite
// file. Unlike the device-token DB, a missing file is NOT fatal: the store
// is created empty and tokens are pushed later via POST /v1/tokens.
func initAccountTokenDB(path string) error {
	db, err := openTokenDB(path) // same single-conn settings as the device pool
	if err != nil {
		return fmt.Errorf("open account token store: %w", err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS account_tokens (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		token        TEXT NOT NULL UNIQUE,
		label        TEXT NOT NULL DEFAULT '',
		created_at   INTEGER NOT NULL,
		last_used_at INTEGER NOT NULL DEFAULT 0,
		fail_count   INTEGER NOT NULL DEFAULT 0,
		disabled     INTEGER NOT NULL DEFAULT 0
	);`)
	if err != nil {
		db.Close()
		return fmt.Errorf("create account_tokens schema: %w", err)
	}
	accountDBMu.Lock()
	old := accountDB
	accountDB = db
	accountDBPath = path
	accountDBMu.Unlock()
	if old != nil && old != db {
		old.Close()
	}
	return nil
}

func withAccountDB(fn func(*sql.DB) error) error {
	accountDBMu.RLock()
	db := accountDB
	accountDBMu.RUnlock()
	if db == nil {
		return ErrAccountTokenStoreUnavailable
	}
	return fn(db)
}

// addAccountToken inserts a token; duplicate token returns the existing id.
func addAccountToken(token, label string) (int64, error) {
	token = strings.TrimSpace(token)
	label = strings.TrimSpace(label)
	if token == "" {
		return 0, errors.New("token is empty")
	}
	var id int64
	err := withAccountDB(func(db *sql.DB) error {
		_, err := db.Exec(
			"INSERT INTO account_tokens (token, label, created_at) VALUES (?, ?, ?) ON CONFLICT(token) DO NOTHING",
			token, label, time.Now().Unix(),
		)
		if err != nil {
			return err
		}
		return db.QueryRow("SELECT id FROM account_tokens WHERE token = ?", token).Scan(&id)
	})
	return id, err
}

type accountTokenRow struct {
	ID         int64  `json:"id"`
	Label      string `json:"label"`
	TokenPrev  string `json:"token_preview"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"`
	FailCount  int64  `json:"fail_count"`
	Disabled   bool   `json:"disabled"`
}

func maskToken(t string) string {
	if len(t) <= 16 {
		return t[:4] + "…"
	}
	return t[:12] + "…" + t[len(t)-4:]
}

func listAccountTokens() ([]accountTokenRow, int, error) {
	var rows []accountTokenRow
	err := withAccountDB(func(db *sql.DB) error {
		r, err := db.Query("SELECT id, token, label, created_at, last_used_at, fail_count, disabled FROM account_tokens ORDER BY id")
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var row accountTokenRow
			var token string
			var disabled int
			if err := r.Scan(&row.ID, &token, &row.Label, &row.CreatedAt, &row.LastUsedAt, &row.FailCount, &disabled); err != nil {
				return err
			}
			row.TokenPrev = maskToken(token)
			row.Disabled = disabled != 0
			rows = append(rows, row)
		}
		return r.Err()
	})
	return rows, len(rows), err
}

func deleteAccountToken(id int64) error {
	return withAccountDB(func(db *sql.DB) error {
		res, err := db.Exec("DELETE FROM account_tokens WHERE id = ?", id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("token id %d not found", id)
		}
		return nil
	})
}

// acquireAccountToken picks the next usable token: not disabled, lowest
// fail_count first, then least-recently-used. Returns (id, token, true) or
// (_, "", false) when the pool is empty/exhausted.
func acquireAccountToken() (int64, string, bool) {
	var id int64
	var token string
	err := withAccountDB(func(db *sql.DB) error {
		return db.QueryRow(
			"SELECT id, token FROM account_tokens WHERE disabled = 0 ORDER BY fail_count ASC, last_used_at ASC, id ASC LIMIT 1",
		).Scan(&id, &token)
	})
	if err != nil || token == "" {
		return 0, "", false
	}
	return id, token, true
}

// markAccountTokenResult records a use outcome. Success resets fail_count and
// stamps last_used_at; failure increments fail_count and auto-disables past
// the threshold so a dead token stops being handed out.
func markAccountTokenResult(id int64, ok bool) {
	if id == 0 {
		return
	}
	_ = withAccountDB(func(db *sql.DB) error {
		if ok {
			return db.QueryRow(
				"UPDATE account_tokens SET last_used_at = ?, fail_count = 0 WHERE id = ? RETURNING id",
				time.Now().Unix(), id,
			).Scan(new(int64))
		}
		return db.QueryRow(
			`UPDATE account_tokens
			 SET fail_count = fail_count + 1,
			     disabled = CASE WHEN fail_count + 1 >= ? THEN 1 ELSE disabled END
			 WHERE id = ? RETURNING id`,
			accountTokenFailDisableThreshold, id,
		).Scan(new(int64))
	})
}

func accountTokenCount() int {
	count := 0
	_ = withAccountDB(func(db *sql.DB) error {
		return db.QueryRow("SELECT COUNT(*) FROM account_tokens WHERE disabled = 0").Scan(&count)
	})
	return count
}
