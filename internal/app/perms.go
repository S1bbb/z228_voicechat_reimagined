package app

import (
	"context"
	"log"
	"strings"
	"time"
)

// Права ролей. Админ по паролю может всё; роль с правом "admin" — тоже.
const (
	PermAdmin    = "admin"    // всё: роли, ИИ, резервные копии и права ниже
	PermAI       = "ai"       // звать ИИ (@ai, /ai)
	PermMessages = "messages" // удалять чужие сообщения
	PermMute     = "mute"     // мьют для всех, отключить от голоса, перетаскивать между каналами
	PermKick     = "kick"     // выгонять и банить
	PermChannels = "channels" // создавать, переименовывать, удалять каналы
	PermInvites  = "invites"  // ключи доступа
)

var allPerms = []string{PermAdmin, PermAI, PermMessages, PermMute, PermKick, PermChannels, PermInvites}

// cmdPerm — какое право нужно для команды управления.
var cmdPerm = map[string]string{
	"channel-create":   PermChannels,
	"channel-rename":   PermChannels,
	"channel-delete":   PermChannels,
	"channel-move":     PermChannels,
	"kick":             PermKick,
	"ban":              PermKick,
	"unban":            PermKick,
	"bans":             PermKick,
	"disconnect-voice": PermMute,
	"server-mute":      PermMute,
	"move-voice":       PermMute,
	"invites":          PermInvites,
	"invite-create":    PermInvites,
	"invite-delete":    PermInvites,
	"role-create":      PermAdmin,
	"role-delete":      PermAdmin,
	"role-move":        PermAdmin,
	"role-assign":      PermAdmin,
	"role-perms":       PermAdmin,
	"backup-download":  PermAdmin,
	"ai-settings":      PermAdmin,
	"ai-config":        PermAdmin,
}

func validPerms(in []string) []string {
	out := []string{}
	for _, p := range allPerms {
		for _, q := range in {
			if q == p {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// can — есть ли у сессии право (админ по паролю или роль с этим правом / правом admin).
func (h *Hub) can(s *session, perm string) bool {
	h.mu.Lock()
	admin, nick := s.admin, s.nickname
	h.mu.Unlock()
	if admin {
		return true
	}
	perms, err := h.opts.Store.PermsOf(context.Background(), nick)
	if err != nil {
		log.Printf("perms: %v", err)
		return false
	}
	for _, p := range perms {
		if p == PermAdmin || p == perm {
			return true
		}
	}
	return false
}

// ---------- баны ----------

func (h *Hub) sendBans(s *session) {
	list, err := h.opts.Store.Bans(context.Background())
	if err != nil {
		s.sendError("Не удалось загрузить баны.")
		return
	}
	s.send("bans", map[string]any{"bans": list})
}

// handleBans: ban {sessionId | name}, unban {id}, bans.
// Бан закрывает ник и браузер, отключает человека и стирает у него ключ; разовый ключ, по которому
// он вошёл, удаляется. Вернуться можно только после разбана — и заново вставив ссылку.
func (h *Hub) handleBans(s *session, m *inMsg) {
	ctx := context.Background()
	st := h.opts.Store
	switch m.Type {
	case "ban":
		var target *session
		nick := ""
		h.mu.Lock()
		if m.SessionID != "" {
			target = h.sessions[m.SessionID]
		} else if n, ok := normalizeNick(m.Name); ok {
			nick = n
			target = h.byNick[strings.ToLower(n)]
		}
		var clientID, code string
		targetAdmin := false
		if target != nil {
			nick, clientID, code, targetAdmin = target.nickname, target.clientID, target.inviteCode, target.admin
		}
		myAdmin := s.admin
		h.mu.Unlock()
		if nick == "" {
			s.sendError("Пользователь не найден.")
			return
		}
		if strings.EqualFold(nick, s.nickname) {
			s.sendError("Себя забанить нельзя.")
			return
		}
		if targetAdmin && !myAdmin {
			s.sendError("Администратора забанить нельзя.")
			return
		}
		if err := st.AddBan(ctx, nick, clientID, s.nickname); err != nil {
			s.sendError("Не удалось забанить.")
			return
		}
		if code != "" {
			if inv, err := st.GetInvite(ctx, code); err == nil && inv.MaxUses == 1 {
				_ = st.DeleteInvite(ctx, code)
			}
		}
		log.Printf("%s забанил %s", s.nickname, nick)
		if target != nil {
			target.send("kicked", map[string]any{"message": "Вас забанил администратор " + s.nickname + ".", "reason": "banned"})
			go func(t *session) {
				time.Sleep(200 * time.Millisecond)
				h.dropSession(t)
				h.broadcastPresence()
			}(target)
		}
		h.broadcast("member-gone", map[string]any{"nickname": nick}, nil)
	case "unban":
		if err := st.RemoveBan(ctx, m.ID); err != nil {
			s.sendError("Не удалось разбанить.")
			return
		}
	}
	h.sendBans(s)
}
