package store

import (
	"context"
	"sort"
	"strings"
)

// Статистика голосовых каналов и память ИИ.
const schemaStats = `
CREATE TABLE IF NOT EXISTS voice_sessions(
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	nick_key     TEXT NOT NULL,
	nick         TEXT NOT NULL,
	channel_id   INTEGER NOT NULL,
	channel_name TEXT NOT NULL,              -- на случай, если канал потом удалят или переименуют
	started_at   INTEGER NOT NULL,
	ended_at     INTEGER NOT NULL            -- пока человек сидит — время последней отметки (раз в минуту)
);
CREATE INDEX IF NOT EXISTS voice_sessions_ended ON voice_sessions(ended_at);
CREATE TABLE IF NOT EXISTS ai_memory(
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	nick_key   TEXT NOT NULL,
	nick       TEXT NOT NULL,
	note       TEXT NOT NULL,
	created_by TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ai_memory_nick ON ai_memory(nick_key);
`

// короче этого сессии не считаем (случайно зашёл и вышел)
const voiceMinMs = 5000

// VoiceStart отмечает вход в голосовой канал и возвращает id сессии.
func (s *Store) VoiceStart(ctx context.Context, nick string, channelID int64, channelName string, at int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO voice_sessions(nick_key,nick,channel_id,channel_name,started_at,ended_at) VALUES(?,?,?,?,?,?)`,
		strings.ToLower(nick), nick, channelID, channelName, at, at)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// VoiceTouch продлевает идущую сессию (чтобы после падения сервера потерялось не больше минуты).
func (s *Store) VoiceTouch(ctx context.Context, id, at int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE voice_sessions SET ended_at=? WHERE id=? AND ended_at<?`, at, id, at)
	return err
}

// VoiceEnd закрывает сессию; совсем короткие удаляет.
func (s *Store) VoiceEnd(ctx context.Context, id, at int64) error {
	if err := s.VoiceTouch(ctx, id, at); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM voice_sessions WHERE id=? AND ended_at-started_at<?`, id, voiceMinMs)
	return err
}

type ChannelTime struct {
	Name string `json:"name"`
	Ms   int64  `json:"ms"`
}

type UserVoiceStats struct {
	Nickname string        `json:"nickname"`
	Ms       int64         `json:"ms"`
	Sessions int           `json:"sessions"`
	Longest  int64         `json:"longest"`
	Channels []ChannelTime `json:"channels"`
}

type ChannelVoiceStats struct {
	Name   string `json:"name"`
	Ms     int64  `json:"ms"`
	People int    `json:"people"`
}

type VoiceStats struct {
	Since    int64               `json:"since"`
	TotalMs  int64               `json:"totalMs"`
	Users    []UserVoiceStats    `json:"users"`
	Channels []ChannelVoiceStats `json:"channels"`
}

// VoiceStats — кто где сколько сидел с момента since (0 — за всё время). active — id идущих сессий,
// у них конец считаем «сейчас». Когда именно кто сидел — наружу не отдаём, только суммы.
func (s *Store) VoiceStats(ctx context.Context, since, now int64, active map[int64]bool) (VoiceStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, nick_key, nick, channel_id, channel_name, started_at, ended_at FROM voice_sessions
		WHERE ended_at>? AND nick_key NOT IN (SELECT nick FROM bans) ORDER BY started_at`, since)
	if err != nil {
		return VoiceStats{}, err
	}
	defer rows.Close()
	type chKey struct {
		user string
		ch   int64
	}
	users := map[string]*UserVoiceStats{}
	userCh := map[chKey]*ChannelTime{}
	chans := map[int64]*ChannelVoiceStats{}
	chPeople := map[int64]map[string]bool{}
	out := VoiceStats{Since: since, Users: []UserVoiceStats{}, Channels: []ChannelVoiceStats{}}
	for rows.Next() {
		var (
			id, chID, start, end int64
			key, nick, chName    string
		)
		if err := rows.Scan(&id, &key, &nick, &chID, &chName, &start, &end); err != nil {
			return VoiceStats{}, err
		}
		live := active[id]
		if live {
			end = now
		}
		from := start
		if from < since {
			from = since
		}
		d := end - from
		if d <= 0 || (!live && end-start < voiceMinMs) {
			continue
		}
		u := users[key]
		if u == nil {
			u = &UserVoiceStats{}
			users[key] = u
		}
		u.Nickname = nick // последнее написание ника
		u.Ms += d
		u.Sessions++
		if d > u.Longest {
			u.Longest = d
		}
		k := chKey{key, chID}
		if userCh[k] == nil {
			userCh[k] = &ChannelTime{}
		}
		userCh[k].Name = chName
		userCh[k].Ms += d
		c := chans[chID]
		if c == nil {
			c = &ChannelVoiceStats{}
			chans[chID] = c
			chPeople[chID] = map[string]bool{}
		}
		c.Name = chName
		c.Ms += d
		chPeople[chID][key] = true
		out.TotalMs += d
	}
	if err := rows.Err(); err != nil {
		return VoiceStats{}, err
	}
	for k, ct := range userCh {
		users[k.user].Channels = append(users[k.user].Channels, *ct)
	}
	for _, u := range users {
		sort.Slice(u.Channels, func(i, j int) bool { return u.Channels[i].Ms > u.Channels[j].Ms })
		out.Users = append(out.Users, *u)
	}
	sort.Slice(out.Users, func(i, j int) bool { return out.Users[i].Ms > out.Users[j].Ms })
	for id, c := range chans {
		c.People = len(chPeople[id])
		out.Channels = append(out.Channels, *c)
	}
	sort.Slice(out.Channels, func(i, j int) bool { return out.Channels[i].Ms > out.Channels[j].Ms })
	return out, nil
}

// ---------- память ИИ ----------

const (
	aiMemPerNick = 8
	aiMemNoteMax = 200
)

type AIMemory struct {
	ID        int64  `json:"id"`
	Nickname  string `json:"nickname"`
	Note      string `json:"note"`
	CreatedBy string `json:"createdBy"`
	CreatedAt int64  `json:"createdAt"`
}

// AIRemember сохраняет заметку о человеке; у одного ника держим не больше aiMemPerNick (старые вытесняются).
func (s *Store) AIRemember(ctx context.Context, nick, note, by string) error {
	note = strings.TrimSpace(note)
	if r := []rune(note); len(r) > aiMemNoteMax {
		note = string(r[:aiMemNoteMax])
	}
	key := strings.ToLower(strings.TrimSpace(nick))
	if key == "" || note == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO ai_memory(nick_key,nick,note,created_by,created_at) VALUES(?,?,?,?,?)`,
		key, strings.TrimSpace(nick), note, by, nowMs()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM ai_memory WHERE nick_key=? AND id NOT IN
		(SELECT id FROM ai_memory WHERE nick_key=? ORDER BY id DESC LIMIT ?)`, key, key, aiMemPerNick)
	return err
}

// AIForget стирает заметки о нике.
func (s *Store) AIForget(ctx context.Context, nick string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ai_memory WHERE nick_key=?`, strings.ToLower(strings.TrimSpace(nick)))
	return err
}

func (s *Store) AIMemoryDelete(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ai_memory WHERE id=?`, id)
	return err
}

// AIMemories — все заметки (свежие сначала).
func (s *Store) AIMemories(ctx context.Context, limit int) ([]AIMemory, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, nick, note, created_by, created_at FROM ai_memory ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AIMemory{}
	for rows.Next() {
		var m AIMemory
		if err := rows.Scan(&m.ID, &m.Nickname, &m.Note, &m.CreatedBy, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
