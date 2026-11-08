package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// Закрепы, опросы, превью ссылок и поиск по сообщениям.

const schemaMsgExtra = `
CREATE TABLE IF NOT EXISTS poll_votes(
	message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	option     INTEGER NOT NULL,
	nick       TEXT NOT NULL,             -- как пишется ник (голоса показываем по нику)
	created_at INTEGER NOT NULL,
	PRIMARY KEY(message_id, option, nick)
);
`

// столбцы messages, добавленные позже (migrate)
var msgExtraColumns = []struct{ table, column, def string }{
	{"messages", "pinned_at", "INTEGER"},
	{"messages", "pinned_by", "TEXT NOT NULL DEFAULT ''"},
	{"messages", "embeds", "TEXT NOT NULL DEFAULT '[]'"},
	{"messages", "poll", "TEXT NOT NULL DEFAULT ''"},
}

type Pin struct {
	By string `json:"by"`
	At int64  `json:"at"`
}

// Embed — превью ссылки (собирает сервер, картинка — внешняя ссылка).
type Embed struct {
	URL         string `json:"url"`
	Kind        string `json:"kind,omitempty"` // "" — карточка, "image" — сама ссылка на картинку, "video" — YouTube и т.п.
	Site        string `json:"site,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Image       string `json:"image,omitempty"`
	Color       string `json:"color,omitempty"`
	VideoID     string `json:"videoId,omitempty"` // YouTube: id ролика для встроенного плеера
}

type PollOption struct {
	Text  string   `json:"text"`
	Votes []string `json:"votes"`
}

type Poll struct {
	Question string       `json:"question"`
	Options  []PollOption `json:"options"`
	Multi    bool         `json:"multi"`
}

// хранимая форма опроса (без голосов)
type pollDef struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Multi    bool     `json:"multi"`
}

const (
	PollMaxOptions = 10
	PollMinOptions = 2
	PollTextMax    = 120
	PollQMax       = 300
)

var ErrBadPoll = errors.New("bad poll")

// CreatePoll — сообщение-опрос.
func (s *Store) CreatePoll(ctx context.Context, channelID int64, author, question string, options []string, multi bool) (Message, error) {
	q := clip(strings.TrimSpace(question), PollQMax)
	var opts []string
	seen := map[string]bool{}
	for _, o := range options {
		o = clip(strings.Join(strings.Fields(o), " "), PollTextMax)
		if o == "" || seen[strings.ToLower(o)] {
			continue
		}
		seen[strings.ToLower(o)] = true
		opts = append(opts, o)
	}
	if q == "" || len(opts) < PollMinOptions || len(opts) > PollMaxOptions {
		return Message{}, ErrBadPoll
	}
	def, _ := json.Marshal(pollDef{Question: q, Options: opts, Multi: multi})
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages(channel_id,author,text,attachments,created_at,poll) VALUES(?,?,?,?,?,?)`,
		channelID, author, "", "[]", nowMs(), string(def))
	if err != nil {
		return Message{}, err
	}
	id, _ := res.LastInsertId()
	return s.Message(ctx, id)
}

// VotePoll переключает голос за вариант; в опросе с одним ответом прошлый голос снимается.
func (s *Store) VotePoll(ctx context.Context, id int64, option int, nick string) (Message, error) {
	m, err := s.Message(ctx, id)
	if err != nil {
		return m, err
	}
	if m.Poll == nil || option < 0 || option >= len(m.Poll.Options) {
		return m, ErrBadPoll
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return m, err
	}
	defer tx.Rollback()
	// голоса этого человека (ник сравниваем в Go: lower() в SQLite не знает кириллицу)
	rows, err := tx.QueryContext(ctx, `SELECT option, nick FROM poll_votes WHERE message_id=?`, id)
	if err != nil {
		return m, err
	}
	type vote struct {
		opt  int
		nick string
	}
	var mine []vote
	for rows.Next() {
		var v vote
		if err := rows.Scan(&v.opt, &v.nick); err != nil {
			rows.Close()
			return m, err
		}
		if strings.EqualFold(v.nick, nick) {
			mine = append(mine, v)
		}
	}
	rows.Close()
	had := false
	for _, v := range mine {
		if v.opt == option {
			had = true
		}
		// повторный клик снимает голос; в опросе с одним ответом снимаем и прошлый выбор
		if v.opt == option || !m.Poll.Multi {
			if _, err := tx.ExecContext(ctx, `DELETE FROM poll_votes WHERE message_id=? AND option=? AND nick=?`, id, v.opt, v.nick); err != nil {
				return m, err
			}
		}
	}
	if !had {
		if _, err := tx.ExecContext(ctx, `INSERT INTO poll_votes(message_id,option,nick,created_at) VALUES(?,?,?,?)`, id, option, nick, nowMs()); err != nil {
			return m, err
		}
	}
	if err := tx.Commit(); err != nil {
		return m, err
	}
	return s.Message(ctx, id)
}

// fillPolls подставляет голоса в опросы.
func (s *Store) fillPolls(ctx context.Context, msgs []Message) error {
	for i := range msgs {
		p := msgs[i].Poll
		if p == nil {
			continue
		}
		rows, err := s.db.QueryContext(ctx, `SELECT option, nick FROM poll_votes WHERE message_id=? ORDER BY created_at`, msgs[i].ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var o int
			var n string
			if err := rows.Scan(&o, &n); err != nil {
				rows.Close()
				return err
			}
			if o >= 0 && o < len(p.Options) {
				p.Options[o].Votes = append(p.Options[o].Votes, n)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

func parsePoll(raw string) *Poll {
	if raw == "" {
		return nil
	}
	var d pollDef
	if json.Unmarshal([]byte(raw), &d) != nil || len(d.Options) == 0 {
		return nil
	}
	p := &Poll{Question: d.Question, Multi: d.Multi}
	for _, o := range d.Options {
		p.Options = append(p.Options, PollOption{Text: o, Votes: []string{}})
	}
	return p
}

// SetPinned закрепляет или открепляет сообщение.
func (s *Store) SetPinned(ctx context.Context, id int64, by string, on bool) (Message, error) {
	var err error
	if on {
		_, err = s.db.ExecContext(ctx, `UPDATE messages SET pinned_at=?, pinned_by=? WHERE id=?`, nowMs(), by, id)
	} else {
		_, err = s.db.ExecContext(ctx, `UPDATE messages SET pinned_at=NULL, pinned_by='' WHERE id=?`, id)
	}
	if err != nil {
		return Message{}, err
	}
	return s.Message(ctx, id)
}

const MaxPins = 50

// Pinned — закреплённые сообщения канала, свежие закрепы сначала.
func (s *Store) Pinned(ctx context.Context, channelID int64) ([]Message, error) {
	return s.queryMessages(ctx, msgSelect+`WHERE m.channel_id=? AND m.pinned_at IS NOT NULL ORDER BY m.pinned_at DESC LIMIT ?`, channelID, MaxPins)
}

// PinCount — сколько закреплено в канале.
func (s *Store) PinCount(ctx context.Context, channelID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE channel_id=? AND pinned_at IS NOT NULL`, channelID).Scan(&n)
	return n, err
}

// SetEmbeds сохраняет превью ссылок; text — текст, по которому они собраны (если сообщение успели
// изменить, старые превью не записываем).
func (s *Store) SetEmbeds(ctx context.Context, id int64, text string, embeds []Embed) (Message, bool, error) {
	if embeds == nil {
		embeds = []Embed{}
	}
	raw, _ := json.Marshal(embeds)
	res, err := s.db.ExecContext(ctx, `UPDATE messages SET embeds=? WHERE id=? AND text=?`, string(raw), id, text)
	if err != nil {
		return Message{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Message{}, false, nil
	}
	m, err := s.Message(ctx, id)
	return m, err == nil, err
}

// SearchHit — найденное сообщение.
type SearchHit = Message

const searchScan = 20000

// Search ищет по тексту (без учёта регистра, в т.ч. кириллица) и/или автору в указанных каналах,
// свежие сначала. Просматривает не больше searchScan последних сообщений.
func (s *Store) Search(ctx context.Context, channels []int64, query, author string, limit int) ([]Message, error) {
	if len(channels) == 0 {
		return []Message{}, nil
	}
	q := strings.ToLower(strings.TrimSpace(query))
	a := strings.ToLower(strings.TrimSpace(author))
	if q == "" && a == "" {
		return []Message{}, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(channels)), ",")
	args := make([]any, 0, len(channels)+1)
	for _, c := range channels {
		args = append(args, c)
	}
	args = append(args, searchScan)
	rows, err := s.db.QueryContext(ctx, `SELECT id, author, text, poll FROM messages WHERE channel_id IN (`+ph+`) ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() && len(ids) < limit {
		var id int64
		var au, text, poll string
		if err := rows.Scan(&id, &au, &text, &poll); err != nil {
			rows.Close()
			return nil, err
		}
		if a != "" && !strings.Contains(strings.ToLower(au), a) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(text), q) && !strings.Contains(strings.ToLower(poll), q) {
			continue
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []Message{}
	for _, id := range ids {
		m, err := s.Message(ctx, id)
		if err == nil {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *Store) queryMessages(ctx context.Context, q string, args ...any) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.fillReactions(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
