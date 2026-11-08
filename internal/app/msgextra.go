package app

import (
	"context"
	"log"
	"strings"

	"zalupa228local/internal/store"
)

// Закрепы, опросы и поиск по сообщениям.

// handleMsgExtra — команды pin, pins, poll-create, poll-vote, search. true — команда обработана.
func (h *Hub) handleMsgExtra(s *session, m *inMsg) bool {
	ctx := context.Background()
	st := h.opts.Store
	switch m.Type {
	case "pin":
		// закреплять может любой, кто видит канал (в ЛС — оба собеседника)
		if !s.allow(1) {
			s.sendError("Слишком часто, подождите немного.")
			return true
		}
		orig, err := st.Message(ctx, m.ID)
		if err != nil {
			s.sendError(storeErr(err, "Сообщение не найдено."))
			return true
		}
		_, dmMembers, ok := h.channelAccess(ctx, s, orig.ChannelID)
		if !ok {
			s.sendError("Канал не найден.")
			return true
		}
		if m.Value && orig.Pinned == nil {
			if n, _ := st.PinCount(ctx, orig.ChannelID); n >= store.MaxPins {
				s.sendError("В канале уже 50 закрепов — открепите что-нибудь старое.")
				return true
			}
		}
		msg, err := st.SetPinned(ctx, m.ID, s.nickname, m.Value)
		if err != nil {
			s.sendError("Не удалось закрепить сообщение.")
			return true
		}
		h.broadcastIn(dmMembers, "message-update", map[string]any{"message": msg}, nil)
		h.broadcastIn(dmMembers, "pins-changed", map[string]any{"channelId": msg.ChannelID, "by": s.nickname, "pinned": m.Value, "id": msg.ID}, nil)

	case "pins":
		if _, _, ok := h.channelAccess(ctx, s, m.ChannelID); !ok {
			s.sendError("Канал не найден.")
			return true
		}
		list, err := st.Pinned(ctx, m.ChannelID)
		if err != nil {
			s.sendError("Не удалось загрузить закреплённые.")
			return true
		}
		s.send("pins", map[string]any{"channelId": m.ChannelID, "messages": list})

	case "poll-create":
		if !s.allow(2) {
			s.sendError("Слишком часто, подождите немного.")
			return true
		}
		_, dmMembers, ok := h.channelAccess(ctx, s, m.ChannelID)
		if !ok {
			s.sendError("Канал не найден.")
			return true
		}
		msg, err := st.CreatePoll(ctx, m.ChannelID, s.nickname, m.Text, m.Keys, m.Value)
		if err != nil {
			if err == store.ErrBadPoll {
				s.sendError("Нужен вопрос и от 2 до 10 разных вариантов.")
			} else {
				s.sendError("Не удалось создать опрос.")
			}
			return true
		}
		h.broadcastIn(dmMembers, "message", map[string]any{"message": msg, "nonce": m.Nonce, "from": s.id}, nil)

	case "poll-vote":
		if !s.allow(0.3) {
			return true
		}
		orig, err := st.Message(ctx, m.ID)
		if err != nil {
			s.sendError(storeErr(err, "Опрос не найден."))
			return true
		}
		_, dmMembers, ok := h.channelAccess(ctx, s, orig.ChannelID)
		if !ok {
			s.sendError("Канал не найден.")
			return true
		}
		msg, err := st.VotePoll(ctx, m.ID, m.Option, s.nickname)
		if err != nil {
			s.sendError("Не удалось проголосовать.")
			return true
		}
		h.broadcastIn(dmMembers, "message-update", map[string]any{"message": msg}, nil)

	case "search":
		if !s.allow(1) {
			s.sendError("Слишком часто, подождите немного.")
			return true
		}
		var chans []int64
		if m.ChannelID != 0 {
			if _, _, ok := h.channelAccess(ctx, s, m.ChannelID); !ok {
				s.sendError("Канал не найден.")
				return true
			}
			chans = []int64{m.ChannelID}
		} else {
			// везде, куда человек может заглянуть: текстовые каналы и его личные переписки
			all, _ := st.Channels(ctx)
			for _, c := range all {
				if c.Kind == "text" {
					chans = append(chans, c.ID)
				}
			}
			dms, _ := st.DMsOf(ctx, s.nickname)
			for _, d := range dms {
				chans = append(chans, d.ID)
			}
		}
		q := strings.TrimSpace(m.Text)
		if r := []rune(q); len(r) > 100 {
			q = string(r[:100])
		}
		hits, err := st.Search(ctx, chans, q, strings.TrimSpace(m.Name), 50)
		if err != nil {
			log.Printf("поиск: %v", err)
			s.sendError("Поиск не удался.")
			return true
		}
		s.send("search-results", map[string]any{"query": q, "author": m.Name, "channelId": m.ChannelID, "messages": hits, "nonce": m.Nonce})

	default:
		return false
	}
	return true
}
