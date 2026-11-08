package app

import (
	"context"
	"log"
	"strings"

	"zalupa228local/internal/store"
)

// Личные сообщения: канал kind="dm" на двоих. В общий список каналов не попадает, сообщения
// получают только его участники.

// channelAccess — можно ли сессии читать и писать в текстовый канал. Для ЛС возвращает участников.
func (h *Hub) channelAccess(ctx context.Context, s *session, id int64) (store.Channel, []string, bool) {
	ch, err := h.opts.Store.Channel(ctx, id)
	if err != nil {
		return ch, nil, false
	}
	switch ch.Kind {
	case "text":
		return ch, nil, true
	case "dm":
		mem, err := h.opts.Store.DMMembers(ctx, id)
		if err != nil {
			return ch, nil, false
		}
		me := strings.ToLower(s.nickname)
		for _, n := range mem {
			if n == me {
				return ch, mem, true
			}
		}
	}
	return ch, nil, false
}

// dmMembersOf — участники ЛС или nil для обычного канала.
func (h *Hub) dmMembersOf(ctx context.Context, channelID int64) []string {
	mem, err := h.opts.Store.DMMembers(ctx, channelID)
	if err != nil || len(mem) == 0 {
		return nil
	}
	return mem
}

// broadcastIn рассылает событие канала: всем — для обычного канала, только участникам — для ЛС.
func (h *Hub) broadcastIn(dmMembers []string, typ string, payload map[string]any, except *session) {
	if dmMembers == nil {
		h.broadcast(typ, payload, except)
		return
	}
	b := encode(typ, payload)
	h.mu.Lock()
	var list []*session
	for _, n := range dmMembers {
		if t := h.byNick[n]; t != nil && t != except {
			list = append(list, t)
		}
	}
	h.mu.Unlock()
	for _, t := range list {
		t.sendRaw(b)
	}
}

// sendDMs отправляет человеку (если он в сети) список его личных каналов.
func (h *Hub) sendDMs(nickKey string) {
	h.mu.Lock()
	t := h.byNick[nickKey]
	h.mu.Unlock()
	if t == nil {
		return
	}
	list, err := h.opts.Store.DMsOf(context.Background(), nickKey)
	if err != nil {
		log.Printf("dms: %v", err)
		return
	}
	t.send("dms", map[string]any{"dms": list})
}

// openDM — команда dm-open {name}: найти или создать ЛС с человеком.
func (h *Hub) openDM(s *session, m *inMsg) {
	ctx := context.Background()
	peer, ok := normalizeNick(m.Name)
	if !ok || strings.EqualFold(peer, s.nickname) || strings.EqualFold(peer, botNick) {
		s.sendError("Написать этому человеку нельзя.")
		return
	}
	real, known, err := h.opts.Store.MemberNick(ctx, peer)
	if err != nil || !known {
		s.sendError("Такого человека нет на сервере.")
		return
	}
	id, err := h.opts.Store.OpenDM(ctx, s.nickname, real)
	if err != nil {
		log.Printf("dm-open: %v", err)
		s.sendError("Не удалось открыть личные сообщения.")
		return
	}
	h.sendDMs(strings.ToLower(s.nickname))
	h.sendDMs(strings.ToLower(real))
	s.send("dm-opened", map[string]any{"id": id})
}
