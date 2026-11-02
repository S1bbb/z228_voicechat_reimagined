// Package store — постоянное хранилище на SQLite (каналы, сообщения, реакции, настройки).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
)

type Channel struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"` // "text" | "voice"
	Name     string `json:"name"`
	Position int    `json:"position"`
}

type Attachment struct {
	URL    string `json:"url"`
	Name   string `json:"name"`
	Mime   string `json:"mime"`
	Size   int64  `json:"size"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

type ReplyRef struct {
	ID     int64  `json:"id"`
	Author string `json:"author"`
	Text   string `json:"text"`
}

type Message struct {
	ID          int64               `json:"id"`
	ChannelID   int64               `json:"channelId"`
	Author      string              `json:"author"`
	Text        string              `json:"text"`
	CreatedAt   int64               `json:"createdAt"` // unix ms
	EditedAt    *int64              `json:"editedAt"`
	ReplyTo     *ReplyRef           `json:"replyTo"`
	Attachments []Attachment        `json:"attachments"`
	Reactions   map[string][]string `json:"reactions"`
}

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS settings(
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS channels(
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	kind       TEXT NOT NULL,
	name       TEXT NOT NULL,
	position   INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS messages(
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	channel_id  INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
	author      TEXT NOT NULL,
	text        TEXT NOT NULL DEFAULT '',
	reply_to    INTEGER,
	attachments TEXT NOT NULL DEFAULT '[]',
	created_at  INTEGER NOT NULL,
	edited_at   INTEGER
);
CREATE INDEX IF NOT EXISTS messages_channel ON messages(channel_id, id);
CREATE TABLE IF NOT EXISTS invites(
	code         TEXT PRIMARY KEY,
	label        TEXT NOT NULL DEFAULT '',
	created_by   TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	uses         INTEGER NOT NULL DEFAULT 0,
	last_used_at INTEGER
);
CREATE TABLE IF NOT EXISTS avatars(
	nick       TEXT PRIMARY KEY, -- ник в нижнем регистре
	url        TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS reactions(
	message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	emoji      TEXT NOT NULL,
	author     TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY(message_id, emoji, author)
);
`

func Open(dataDir string) (*Store, error) {
	path := filepath.Join(dataDir, "z228.db")
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Один писатель — никаких SQLITE_BUSY; для чата на десятки человек этого с запасом.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	s := &Store{db: db}
	if err := s.seed(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) seed(ctx context.Context) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channels`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	for _, ch := range []struct{ kind, name string }{
		{"text", "пишущая залупа"},
		{"text", "фото залупы"},
		{"voice", "говорящая залупа"},
		{"voice", "шепчущая залупа"},
		{"voice", "молчащая залупа"},
	} {
		if _, err := s.CreateChannel(ctx, ch.kind, ch.name); err != nil {
			return err
		}
	}
	return nil
}

// ---------- settings ----------

func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ---------- invites (ключи доступа) ----------

type Invite struct {
	Code       string `json:"code"`
	Label      string `json:"label"`
	CreatedBy  string `json:"createdBy"`
	CreatedAt  int64  `json:"createdAt"`
	Uses       int    `json:"uses"`
	LastUsedAt *int64 `json:"lastUsedAt"`
}

func (s *Store) Invites(ctx context.Context) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT code, label, created_by, created_at, uses, last_used_at FROM invites ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Invite{}
	for rows.Next() {
		var in Invite
		var last sql.NullInt64
		if err := rows.Scan(&in.Code, &in.Label, &in.CreatedBy, &in.CreatedAt, &in.Uses, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			v := last.Int64
			in.LastUsedAt = &v
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *Store) InviteCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM invites`).Scan(&n)
	return n, err
}

func (s *Store) CreateInvite(ctx context.Context, code, label, by string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO invites(code,label,created_by,created_at) VALUES(?,?,?,?)`, code, label, by, nowMs())
	return err
}

func (s *Store) DeleteInvite(ctx context.Context, code string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM invites WHERE code=?`, code)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UseInvite отмечает вход по ключу; false — такого ключа нет.
func (s *Store) UseInvite(ctx context.Context, code string) (bool, error) {
	if code == "" {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx, `UPDATE invites SET uses=uses+1, last_used_at=? WHERE code=?`, nowMs(), code)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---------- avatars ----------

// Avatars — все аватарки: ник в нижнем регистре -> URL картинки.
func (s *Store) Avatars(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT nick, url FROM avatars`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var nick, url string
		if err := rows.Scan(&nick, &url); err != nil {
			return nil, err
		}
		out[nick] = url
	}
	return out, rows.Err()
}

// SetAvatar ставит аватарку нику; пустой url — убрать.
func (s *Store) SetAvatar(ctx context.Context, nick, url string) error {
	nick = strings.ToLower(nick)
	if url == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM avatars WHERE nick=?`, nick)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO avatars(nick,url,updated_at) VALUES(?,?,?) ON CONFLICT(nick) DO UPDATE SET url=excluded.url, updated_at=excluded.updated_at`,
		nick, url, nowMs())
	return err
}

// ---------- channels ----------

func (s *Store) Channels(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, name, position FROM channels ORDER BY kind DESC, position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Channel{}
	for rows.Next() {
		var c Channel
		if err := rows.Scan(&c.ID, &c.Kind, &c.Name, &c.Position); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Channel(ctx context.Context, id int64) (Channel, error) {
	var c Channel
	err := s.db.QueryRowContext(ctx, `SELECT id, kind, name, position FROM channels WHERE id=?`, id).
		Scan(&c.ID, &c.Kind, &c.Name, &c.Position)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

func (s *Store) CreateChannel(ctx context.Context, kind, name string) (Channel, error) {
	var pos int
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),0)+1 FROM channels WHERE kind=?`, kind).Scan(&pos)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO channels(kind,name,position,created_at) VALUES(?,?,?,?)`, kind, name, pos, nowMs())
	if err != nil {
		return Channel{}, err
	}
	id, _ := res.LastInsertId()
	return Channel{ID: id, Kind: kind, Name: name, Position: pos}, nil
}

func (s *Store) RenameChannel(ctx context.Context, id int64, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE channels SET name=? WHERE id=?`, name, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteChannel(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM channels WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MoveChannel сдвигает канал на одну позицию вверх (dir<0) или вниз (dir>0) внутри своего типа.
func (s *Store) MoveChannel(ctx context.Context, id int64, dir int) error {
	chs, err := s.Channels(ctx)
	if err != nil {
		return err
	}
	target, err := s.Channel(ctx, id)
	if err != nil {
		return err
	}
	var same []Channel
	idx := -1
	for _, c := range chs {
		if c.Kind != target.Kind {
			continue
		}
		if c.ID == id {
			idx = len(same)
		}
		same = append(same, c)
	}
	j := idx + dir
	if idx < 0 || j < 0 || j >= len(same) {
		return nil
	}
	same[idx], same[j] = same[j], same[idx]
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, c := range same {
		if _, err := tx.ExecContext(ctx, `UPDATE channels SET position=? WHERE id=?`, i, c.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------- messages ----------

const msgSelect = `
SELECT m.id, m.channel_id, m.author, m.text, m.attachments, m.created_at, m.edited_at,
       r.id, r.author, r.text
FROM messages m
LEFT JOIN messages r ON r.id = m.reply_to
`

func scanMessage(sc interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var att string
	var edited sql.NullInt64
	var rID sql.NullInt64
	var rAuthor, rText sql.NullString
	if err := sc.Scan(&m.ID, &m.ChannelID, &m.Author, &m.Text, &att, &m.CreatedAt, &edited, &rID, &rAuthor, &rText); err != nil {
		return m, err
	}
	if edited.Valid {
		v := edited.Int64
		m.EditedAt = &v
	}
	if rID.Valid {
		txt := rText.String
		if r := []rune(txt); len(r) > 120 {
			txt = string(r[:120]) + "…"
		}
		m.ReplyTo = &ReplyRef{ID: rID.Int64, Author: rAuthor.String, Text: txt}
	}
	m.Attachments = []Attachment{}
	_ = json.Unmarshal([]byte(att), &m.Attachments)
	m.Reactions = map[string][]string{}
	return m, nil
}

// History возвращает до limit сообщений канала с id < before (before=0 — самые свежие), в хронологическом порядке.
func (s *Store) History(ctx context.Context, channelID, before int64, limit int) ([]Message, bool, error) {
	q := msgSelect + `WHERE m.channel_id=? AND (?=0 OR m.id<?) ORDER BY m.id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, channelID, before, before, limit+1)
	if err != nil {
		return nil, false, err
	}
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return nil, false, err
		}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if err := s.fillReactions(ctx, out); err != nil {
		return nil, false, err
	}
	if out == nil {
		out = []Message{}
	}
	return out, hasMore, nil
}

func (s *Store) fillReactions(ctx context.Context, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	idx := make(map[int64]int, len(msgs))
	args := make([]any, 0, len(msgs))
	for i, m := range msgs {
		idx[m.ID] = i
		args = append(args, m.ID)
	}
	q := `SELECT message_id, emoji, author FROM reactions WHERE message_id IN (?` +
		strings.Repeat(",?", len(args)-1) + `) ORDER BY created_at`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var emoji, author string
		if err := rows.Scan(&id, &emoji, &author); err != nil {
			return err
		}
		m := &msgs[idx[id]]
		m.Reactions[emoji] = append(m.Reactions[emoji], author)
	}
	return rows.Err()
}

func (s *Store) Message(ctx context.Context, id int64) (Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx, msgSelect+`WHERE m.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, err
	}
	one := []Message{m}
	if err := s.fillReactions(ctx, one); err != nil {
		return m, err
	}
	return one[0], nil
}

func (s *Store) CreateMessage(ctx context.Context, channelID int64, author, text string, replyTo int64, atts []Attachment) (Message, error) {
	if atts == nil {
		atts = []Attachment{}
	}
	attJSON, _ := json.Marshal(atts)
	var reply any
	if replyTo > 0 {
		var ch int64
		if err := s.db.QueryRowContext(ctx, `SELECT channel_id FROM messages WHERE id=?`, replyTo).Scan(&ch); err == nil && ch == channelID {
			reply = replyTo
		}
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages(channel_id,author,text,reply_to,attachments,created_at) VALUES(?,?,?,?,?,?)`,
		channelID, author, text, reply, string(attJSON), nowMs())
	if err != nil {
		return Message{}, err
	}
	id, _ := res.LastInsertId()
	return s.Message(ctx, id)
}

// EditMessage меняет текст; править может только автор.
func (s *Store) EditMessage(ctx context.Context, id int64, author, text string) (Message, error) {
	m, err := s.Message(ctx, id)
	if err != nil {
		return m, err
	}
	if m.Author != author {
		return m, ErrForbidden
	}
	if strings.TrimSpace(text) == "" && len(m.Attachments) == 0 {
		return m, errors.New("empty")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE messages SET text=?, edited_at=? WHERE id=?`, text, nowMs(), id); err != nil {
		return m, err
	}
	return s.Message(ctx, id)
}

// DeleteMessage удаляет сообщение; может автор или админ (force).
func (s *Store) DeleteMessage(ctx context.Context, id int64, author string, force bool) (Message, error) {
	m, err := s.Message(ctx, id)
	if err != nil {
		return m, err
	}
	if !force && m.Author != author {
		return m, ErrForbidden
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM messages WHERE id=?`, id)
	return m, err
}

// ToggleReaction добавляет/убирает реакцию и возвращает обновлённое сообщение.
func (s *Store) ToggleReaction(ctx context.Context, id int64, emoji, author string) (Message, error) {
	if _, err := s.Message(ctx, id); err != nil {
		return Message{}, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM reactions WHERE message_id=? AND emoji=? AND author=?`, id, emoji, author)
	if err != nil {
		return Message{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO reactions(message_id,emoji,author,created_at) VALUES(?,?,?,?)`, id, emoji, author, nowMs()); err != nil {
			return Message{}, err
		}
	}
	return s.Message(ctx, id)
}

func nowMs() int64 { return time.Now().UnixMilli() }
