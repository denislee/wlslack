package slack

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// MessageRetention is how long message rows are kept on disk before being
// pruned. Anything older than this is dropped on startup and after writes.
const MessageRetention = 7 * 24 * time.Hour

type sqliteStore struct {
	db *sql.DB
}

func openSQLiteStore() (*sqliteStore, error) {
	path := filepath.Join(cacheDir(), "cache.db")
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := initSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &sqliteStore{db: db}, nil
}

func initSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS channels (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			user_id TEXT,
			is_im INTEGER NOT NULL DEFAULT 0,
			is_mpim INTEGER NOT NULL DEFAULT 0,
			is_private INTEGER NOT NULL DEFAULT 0,
			is_external INTEGER NOT NULL DEFAULT 0,
			topic TEXT,
			purpose TEXT,
			unread_count INTEGER NOT NULL DEFAULT 0,
			last_read_ts TEXT,
			latest_ts TEXT,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_channels_name ON channels(name)`,
		`CREATE TABLE IF NOT EXISTS messages (
			channel_id TEXT NOT NULL,
			thread_ts TEXT NOT NULL DEFAULT '',
			ts TEXT NOT NULL,
			ts_unix INTEGER NOT NULL,
			user_id TEXT,
			username TEXT,
			body TEXT,
			reply_count INTEGER NOT NULL DEFAULT 0,
			reactions TEXT,
			edited INTEGER NOT NULL DEFAULT 0,
			edited_ts TEXT,
			edit_history TEXT,
			files TEXT,
			is_bot INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (channel_id, thread_ts, ts)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_channel_ts ON messages(channel_id, thread_ts, ts_unix)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_ts_unix ON messages(ts_unix)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}
	}
	// Tolerant column migrations for installs that pre-date a column. SQLite
	// returns "duplicate column name" when the column is already there; we
	// swallow that since CREATE TABLE IF NOT EXISTS won't add it for us.
	migrations := []string{
		`ALTER TABLE channels ADD COLUMN is_external INTEGER NOT NULL DEFAULT 0`,
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migrate schema: %w", err)
		}
	}
	return nil
}

func (s *sqliteStore) close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// loadAllChannels reads every persisted channel row. Used at startup to
// populate the in-memory map so channel-id → name lookups are immediate.
func (s *sqliteStore) loadAllChannels() ([]Channel, error) {
	rows, err := s.db.Query(`
		SELECT id, name, user_id, is_im, is_mpim, is_private, is_external,
		       topic, purpose, unread_count, last_read_ts, latest_ts
		FROM channels`)
	if err != nil {
		return nil, fmt.Errorf("load channels: %w", err)
	}
	defer rows.Close()

	var out []Channel
	for rows.Next() {
		var ch Channel
		var userID, topic, purpose, lastRead, latest sql.NullString
		var isIM, isMPIM, isPrivate, isExternal int
		if err := rows.Scan(&ch.ID, &ch.Name, &userID, &isIM, &isMPIM, &isPrivate, &isExternal,
			&topic, &purpose, &ch.UnreadCount, &lastRead, &latest); err != nil {
			return nil, err
		}
		ch.UserID = userID.String
		ch.IsIM = isIM != 0
		ch.IsMPIM = isMPIM != 0
		ch.IsPrivate = isPrivate != 0
		ch.IsExternal = isExternal != 0
		ch.Topic = topic.String
		ch.Purpose = purpose.String
		ch.LastReadTS = lastRead.String
		ch.LatestTS = latest.String
		out = append(out, ch)
	}
	return out, rows.Err()
}

func (s *sqliteStore) saveChannels(channels []Channel) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO channels (id, name, user_id, is_im, is_mpim, is_private, is_external,
		                     topic, purpose, unread_count, last_read_ts, latest_ts, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name,
			user_id=excluded.user_id,
			is_im=excluded.is_im,
			is_mpim=excluded.is_mpim,
			is_private=excluded.is_private,
			is_external=excluded.is_external,
			topic=excluded.topic,
			purpose=excluded.purpose,
			unread_count=excluded.unread_count,
			last_read_ts=excluded.last_read_ts,
			latest_ts=excluded.latest_ts,
			updated_at=excluded.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().Unix()
	for _, ch := range channels {
		if _, err := stmt.Exec(ch.ID, ch.Name, ch.UserID,
			boolToInt(ch.IsIM), boolToInt(ch.IsMPIM), boolToInt(ch.IsPrivate), boolToInt(ch.IsExternal),
			ch.Topic, ch.Purpose, ch.UnreadCount, ch.LastReadTS, ch.LatestTS, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *sqliteStore) updateChannelUnread(id string, unread int, lastRead, latest string) error {
	if latest == "" {
		_, err := s.db.Exec(`UPDATE channels SET unread_count=?, last_read_ts=? WHERE id=?`,
			unread, lastRead, id)
		return err
	}
	_, err := s.db.Exec(`UPDATE channels SET unread_count=?, last_read_ts=?, latest_ts=? WHERE id=?`,
		unread, lastRead, latest, id)
	return err
}

// loadMessages returns top-level messages (thread_ts='') for a channel that
// are newer than `since` (a unix timestamp). Pass since=0 to get everything
// retained. Returned messages are ordered ascending by ts.
func (s *sqliteStore) loadMessages(channelID, threadTS string, since int64) ([]Message, error) {
	rows, err := s.db.Query(`
		SELECT ts, user_id, username, body, reply_count, reactions,
		       edited, edited_ts, edit_history, files, is_bot
		FROM messages
		WHERE channel_id = ? AND thread_ts = ? AND ts_unix >= ?
		ORDER BY ts ASC`,
		channelID, threadTS, since)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		var userID, username, body, reactions, editedTS, history, files sql.NullString
		var edited, isBot int
		if err := rows.Scan(&m.Timestamp, &userID, &username, &body, &m.ReplyCount,
			&reactions, &edited, &editedTS, &history, &files, &isBot); err != nil {
			return nil, err
		}
		m.UserID = userID.String
		m.Username = username.String
		m.Text = body.String
		m.Edited = edited != 0
		m.EditedTS = editedTS.String
		m.IsBot = isBot != 0
		if threadTS != "" {
			m.ThreadTS = threadTS
		}
		if reactions.Valid && reactions.String != "" {
			_ = json.Unmarshal([]byte(reactions.String), &m.Reactions)
		}
		if history.Valid && history.String != "" {
			_ = json.Unmarshal([]byte(history.String), &m.EditHistory)
		}
		if files.Valid && files.String != "" {
			_ = json.Unmarshal([]byte(files.String), &m.Files)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *sqliteStore) saveMessages(channelID, threadTS string, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO messages (channel_id, thread_ts, ts, ts_unix, user_id, username,
		                     body, reply_count, reactions, edited, edited_ts,
		                     edit_history, files, is_bot)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(channel_id, thread_ts, ts) DO UPDATE SET
			user_id=excluded.user_id,
			username=excluded.username,
			body=excluded.body,
			reply_count=excluded.reply_count,
			reactions=excluded.reactions,
			edited=excluded.edited,
			edited_ts=excluded.edited_ts,
			edit_history=excluded.edit_history,
			files=excluded.files,
			is_bot=excluded.is_bot`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, m := range msgs {
		reactions, _ := json.Marshal(m.Reactions)
		history, _ := json.Marshal(m.EditHistory)
		files, _ := json.Marshal(m.Files)
		if _, err := stmt.Exec(channelID, threadTS, m.Timestamp, slackTSToUnix(m.Timestamp),
			m.UserID, m.Username, m.Text, m.ReplyCount, string(reactions),
			boolToInt(m.Edited), m.EditedTS, string(history), string(files),
			boolToInt(m.IsBot)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// pruneOldMessages drops every message row older than the retention cutoff.
// Returns the number of rows removed.
func (s *sqliteStore) pruneOldMessages() (int64, error) {
	cutoff := time.Now().Add(-MessageRetention).Unix()
	res, err := s.db.Exec(`DELETE FROM messages WHERE ts_unix < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune messages: %w", err)
	}
	return res.RowsAffected()
}

// slackTSToUnix turns a Slack timestamp ("1700000000.123456") into a unix
// epoch-second integer. Falls back to 0 when the string is malformed —
// such rows will get pruned on next sweep, which is the correct behavior.
func slackTSToUnix(ts string) int64 {
	if ts == "" {
		return 0
	}
	intPart, _, _ := strings.Cut(ts, ".")
	n, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
