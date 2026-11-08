package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Таблицы и столбцы, добавленные после первой версии. CREATE … IF NOT EXISTS безопасно повторять,
// а ALTER TABLE ADD COLUMN — нет, поэтому столбцы добавляем с проверкой (migrate).
const schemaExtra = `
CREATE TABLE IF NOT EXISTS invite_clients(
	code      TEXT NOT NULL REFERENCES invites(code) ON DELETE CASCADE,
	client_id TEXT NOT NULL,
	nick      TEXT NOT NULL DEFAULT '',
	first_at  INTEGER NOT NULL,
	PRIMARY KEY(code, client_id)
);
CREATE TABLE IF NOT EXISTS bans(
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	nick       TEXT NOT NULL,              -- ник в нижнем регистре
	client_id  TEXT NOT NULL DEFAULT '',   -- браузер (может быть пустым — бан только по нику)
	created_by TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS members(
	nick_key   TEXT PRIMARY KEY,           -- ник в нижнем регистре
	nick       TEXT NOT NULL,
	first_seen INTEGER NOT NULL,
	last_seen  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS dm_members(
	channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
	nick       TEXT NOT NULL,              -- ник в нижнем регистре
	PRIMARY KEY(channel_id, nick)
);
CREATE INDEX IF NOT EXISTS dm_members_nick ON dm_members(nick);
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaExtra+schemaStats+schemaMsgExtra); err != nil {
		return err
	}
	for _, c := range append([]struct{ table, column, def string }{
		{"invites", "max_uses", "INTEGER NOT NULL DEFAULT 0"}, // 0 — без ограничения, 1 — разовый
		{"invites", "expires_at", "INTEGER"},                  // NULL — бессрочный
		{"roles", "perms", "TEXT NOT NULL DEFAULT ''"},        // права через запятую
	}, msgExtraColumns...) {
		has, err := s.hasColumn(ctx, c.table, c.column)
		if err != nil {
			return err
		}
		if !has {
			if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+c.table+" ADD COLUMN "+c.column+" "+c.def); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) hasColumn(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ---------- ключи: разовые и временные ----------

// InviteResult — итог проверки ключа при входе.
type InviteResult int

const (
	InviteOK InviteResult = iota
	InviteNotFound
	InviteExpired
	InviteUsedUp // разовый ключ уже привязан к другому человеку
)

// GetInvite — один ключ по коду.
func (s *Store) GetInvite(ctx context.Context, code string) (Invite, error) {
	list, err := s.Invites(ctx)
	if err != nil {
		return Invite{}, err
	}
	for _, in := range list {
		if in.Code == code {
			return in, nil
		}
	}
	return Invite{}, ErrNotFound
}

// UseInvite проверяет ключ и отмечает вход. Ключ с max_uses привязывается к браузерам (clientID):
// свой человек переподключается сколько угодно, а новых пускает не больше max_uses.
func (s *Store) UseInvite(ctx context.Context, code, clientID, nick string) (InviteResult, error) {
	if code == "" {
		return InviteNotFound, nil
	}
	var maxUses int
	var exp sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT max_uses, expires_at FROM invites WHERE code=?`, code).Scan(&maxUses, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return InviteNotFound, nil
	}
	if err != nil {
		return InviteNotFound, err
	}
	now := nowMs()
	if exp.Valid && exp.Int64 <= now {
		return InviteExpired, nil
	}
	var known int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM invite_clients WHERE code=? AND client_id=?`, code, clientID).Scan(&known); err != nil {
		return InviteNotFound, err
	}
	if known == 0 {
		if maxUses > 0 {
			var people int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM invite_clients WHERE code=?`, code).Scan(&people); err != nil {
				return InviteNotFound, err
			}
			if people >= maxUses {
				return InviteUsedUp, nil
			}
		}
		if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO invite_clients(code,client_id,nick,first_at) VALUES(?,?,?,?)`, code, clientID, nick, now); err != nil {
			return InviteNotFound, err
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE invites SET uses=uses+1, last_used_at=? WHERE code=?`, now, code)
	return InviteOK, err
}

// ---------- баны ----------

type Ban struct {
	ID        int64  `json:"id"`
	Nick      string `json:"nick"`
	CreatedBy string `json:"createdBy"`
	CreatedAt int64  `json:"createdAt"`
}

func (s *Store) AddBan(ctx context.Context, nick, clientID, by string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO bans(nick,client_id,created_by,created_at) VALUES(?,?,?,?)`,
		strings.ToLower(nick), clientID, by, nowMs())
	return err
}

func (s *Store) RemoveBan(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM bans WHERE id=?`, id)
	return err
}

func (s *Store) Bans(ctx context.Context) ([]Ban, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT b.id, COALESCE(m.nick, b.nick), b.created_by, b.created_at
		FROM bans b LEFT JOIN members m ON m.nick_key=b.nick ORDER BY b.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Ban{}
	for rows.Next() {
		var b Ban
		if err := rows.Scan(&b.ID, &b.Nick, &b.CreatedBy, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// IsBanned — забанен ли ник или браузер.
func (s *Store) IsBanned(ctx context.Context, nick, clientID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bans WHERE nick=? OR (client_id<>'' AND client_id=?)`,
		strings.ToLower(nick), clientID).Scan(&n)
	return n > 0, err
}

// ---------- участники (кто когда был в сети) ----------

type Member struct {
	Nickname string `json:"nickname"`
	LastSeen int64  `json:"lastSeen"`
}

// Seen отмечает, что ник сейчас в сети (вход или выход).
func (s *Store) Seen(ctx context.Context, nick string) (Member, error) {
	now := nowMs()
	_, err := s.db.ExecContext(ctx, `INSERT INTO members(nick_key,nick,first_seen,last_seen) VALUES(?,?,?,?)
		ON CONFLICT(nick_key) DO UPDATE SET nick=excluded.nick, last_seen=excluded.last_seen`,
		strings.ToLower(nick), nick, now, now)
	return Member{Nickname: nick, LastSeen: now}, err
}

// Members — кто бывал на сервере, свежие сначала (без забаненных).
func (s *Store) Members(ctx context.Context, limit int) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT nick, last_seen FROM members
		WHERE nick_key NOT IN (SELECT nick FROM bans) ORDER BY last_seen DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.Nickname, &m.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MemberNick — ник в исходном написании (для ЛС); ok=false, если человек не бывал на сервере.
func (s *Store) MemberNick(ctx context.Context, nick string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT nick FROM members WHERE nick_key=?`, strings.ToLower(nick)).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// ---------- права ролей ----------

func joinPerms(p []string) string { return strings.Join(p, ",") }

func splitPerms(v string) []string {
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Store) SetRolePerms(ctx context.Context, id int64, perms []string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE roles SET perms=? WHERE id=?`, joinPerms(perms), id)
	return err
}

// PermsOf — права роли, назначенной нику.
func (s *Store) PermsOf(ctx context.Context, nick string) ([]string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT r.perms FROM user_roles u JOIN roles r ON r.id=u.role_id WHERE u.nick=?`,
		strings.ToLower(nick)).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return splitPerms(v), err
}

// ---------- личные сообщения ----------

type DM struct {
	ID     int64  `json:"id"`
	Peer   string `json:"peer"`
	LastID int64  `json:"lastId"`
}

// OpenDM находит или создаёт личный канал двух людей (ники в нижнем регистре).
func (s *Store) OpenDM(ctx context.Context, a, b string) (int64, error) {
	a, b = strings.ToLower(a), strings.ToLower(b)
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT x.channel_id FROM dm_members x JOIN dm_members y ON y.channel_id=x.channel_id
		WHERE x.nick=? AND y.nick=? LIMIT 1`, a, b).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO channels(kind,name,position,created_at) VALUES('dm','',0,?)`, nowMs())
	if err != nil {
		return 0, err
	}
	id, _ = res.LastInsertId()
	for _, n := range []string{a, b} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO dm_members(channel_id,nick) VALUES(?,?)`, id, n); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// DMsOf — личные каналы ника: собеседник и id последнего сообщения (свежие сначала).
func (s *Store) DMsOf(ctx context.Context, nick string) ([]DM, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.channel_id, COALESCE(m.nick, o.nick),
			COALESCE((SELECT MAX(id) FROM messages WHERE channel_id=d.channel_id), 0) AS last
		FROM dm_members d
		JOIN dm_members o ON o.channel_id=d.channel_id AND o.nick<>d.nick
		LEFT JOIN members m ON m.nick_key=o.nick
		WHERE d.nick=? ORDER BY last DESC, d.channel_id DESC`, strings.ToLower(nick))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DM{}
	for rows.Next() {
		var d DM
		if err := rows.Scan(&d.ID, &d.Peer, &d.LastID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DMMembers — участники личного канала (ники в нижнем регистре); пусто — канал не личный.
func (s *Store) DMMembers(ctx context.Context, channelID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT nick FROM dm_members WHERE channel_id=?`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
