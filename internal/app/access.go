package app

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"zalupa228local/internal/store"
)

// Ключи доступа. Сервер закрыт, если задан общий ключ (SERVER_PASSWORD / -password)
// или админ создал хотя бы один ключ-приглашение. Пока ключей нет — вход открыт.

const (
	failWindow = 10 * time.Minute
	failLimit  = 8
	blockFor   = 15 * time.Minute
)

// guard считает неудачные попытки (ключ, пароль админа) по IP и временно блокирует перебор.
type guard struct {
	mu sync.Mutex
	m  map[string]*attempts
}

type attempts struct {
	n     int
	first time.Time
	until time.Time
}

func newGuard() *guard { return &guard{m: map[string]*attempts{}} }

func (g *guard) blocked(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	a := g.m[ip]
	return a != nil && time.Now().Before(a.until)
}

// fail возвращает true, если после этой попытки IP заблокирован.
func (g *guard) fail(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	a := g.m[ip]
	if a == nil || now.Sub(a.first) > failWindow {
		a = &attempts{first: now}
		g.m[ip] = a
	}
	a.n++
	if a.n >= failLimit {
		a.until = now.Add(blockFor)
		a.n = 0
		a.first = now
		return true
	}
	// заодно чистим старые записи, чтобы карта не росла бесконечно
	if len(g.m) > 10000 {
		for k, v := range g.m {
			if now.Sub(v.first) > failWindow && now.After(v.until) {
				delete(g.m, k)
			}
		}
	}
	return false
}

// clientIP — адрес клиента; X-Forwarded-For учитываем только от reverse proxy на этой же машине.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	return host
}

// accessRequired — нужен ли ключ для входа.
func (h *Hub) accessRequired(ctx context.Context) bool {
	if h.opts.ServerPassword != "" {
		return true
	}
	n, err := h.opts.Store.InviteCount(ctx)
	if err != nil {
		log.Printf("invites: %v", err)
		return true // при ошибке БД лучше закрыться, чем открыться
	}
	return n > 0
}

const msgBadKey = "Неверный ключ доступа."

// checkKey проверяет ключ; возвращает код приглашения ("" для общего ключа) и текст отказа ("" — пускаем).
func (h *Hub) checkKey(ctx context.Context, key, clientID, nick string) (string, string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", msgBadKey
	}
	if h.opts.ServerPassword != "" && subtle.ConstantTimeCompare([]byte(key), []byte(h.opts.ServerPassword)) == 1 {
		return "", ""
	}
	code := strings.ToLower(key)
	res, err := h.opts.Store.UseInvite(ctx, code, clientID, nick)
	if err != nil {
		log.Printf("invite: %v", err)
		return "", msgBadKey
	}
	switch res {
	case store.InviteOK:
		return code, ""
	case store.InviteExpired:
		return "", "Срок действия ключа истёк. Попросите у админа новую ссылку."
	case store.InviteUsedUp:
		return "", "Это разовая ссылка, и по ней уже вошёл другой человек. Попросите у админа свою."
	}
	return "", msgBadKey
}

// inviteCode — 16 символов без похожих друг на друга букв/цифр (~79 бит).
func inviteCode() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func (h *Hub) sendInvites(s *session) {
	ctx := context.Background()
	list, err := h.opts.Store.Invites(ctx)
	if err != nil {
		s.sendError("Не удалось загрузить ключи.")
		return
	}
	s.send("invites", map[string]any{
		"invites":  list,
		"required": h.accessRequired(ctx),
		"master":   h.opts.ServerPassword != "",
	})
}

func (h *Hub) handleInvites(s *session, m *inMsg) {
	ctx := context.Background()
	switch m.Type {
	case "invite-create":
		label := strings.Join(strings.Fields(m.Name), " ")
		if r := []rune(label); len(r) > 40 {
			label = string(r[:40])
		}
		maxUses := max(0, min(m.MaxUses, 1000))
		var expires int64
		if m.TTL > 0 {
			expires = time.Now().Add(time.Duration(min(m.TTL, 366*24*3600)) * time.Second).UnixMilli()
		}
		if err := h.opts.Store.CreateInvite(ctx, inviteCode(), label, s.nickname, maxUses, expires); err != nil {
			s.sendError("Не удалось создать ключ.")
			return
		}
		log.Printf("%s создал ключ доступа %q", s.nickname, label)
	case "invite-delete":
		code := strings.ToLower(strings.TrimSpace(m.Text))
		if err := h.opts.Store.DeleteInvite(ctx, code); err != nil {
			s.sendError("Ключ не найден.")
			return
		}
		log.Printf("%s удалил ключ доступа", s.nickname)
		// всех, кто вошёл по отозванному ключу, отключаем
		h.mu.Lock()
		var victims []*session
		for _, x := range h.sessions {
			if x.inviteCode == code && x != s {
				victims = append(victims, x)
			}
		}
		h.mu.Unlock()
		for _, v := range victims {
			v.send("kicked", map[string]any{"message": "Ваш ключ доступа отозван администратором.", "reason": "key"})
			go func(v *session) {
				time.Sleep(200 * time.Millisecond)
				h.dropSession(v)
				h.broadcastPresence()
			}(v)
		}
	}
	h.sendInvites(s)
}

var roleColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// handleRoles — роли: создание, удаление, порядок, назначение (одна роль на ник).
func (h *Hub) handleRoles(s *session, m *inMsg) {
	ctx := context.Background()
	st := h.opts.Store
	var err error
	switch m.Type {
	case "role-create":
		name := strings.Join(strings.Fields(m.Name), " ")
		if r := []rune(name); len(r) > 32 {
			name = string(r[:32])
		}
		color := strings.TrimSpace(m.Text)
		if !roleColor.MatchString(color) {
			color = "#99aab5"
		}
		if name == "" {
			s.sendError("Укажите название роли.")
			return
		}
		err = st.CreateRole(ctx, name, strings.ToLower(color), validPerms(m.Keys))
	case "role-perms":
		err = st.SetRolePerms(ctx, m.ID, validPerms(m.Keys))
	case "role-delete":
		err = st.DeleteRole(ctx, m.ID)
	case "role-move":
		err = st.MoveRole(ctx, m.ID, m.Dir)
	case "role-assign":
		nick, ok := normalizeNick(m.Name)
		if !ok {
			s.sendError("Некорректный ник.")
			return
		}
		err = st.AssignRole(ctx, nick, m.ID)
	}
	if err != nil {
		log.Printf("roles: %v", err)
		s.sendError("Не удалось изменить роли.")
		return
	}
	roles, userRoles, err := st.Roles(ctx)
	if err != nil {
		return
	}
	h.broadcast("roles", map[string]any{"roles": roles, "userRoles": userRoles}, nil)
}
