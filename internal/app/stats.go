package app

import (
	"context"
	"log"
	"time"
)

// Статистика голосовых каналов: каждая посиделка в канале — строка voice_sessions.
// Раз в минуту продлеваем идущие сессии, чтобы при падении сервера терялось не больше минуты.

const voiceTouchEvery = time.Minute

// trackVoice — человек теперь в голосовом канале ch (0 — вышел из голоса).
func (h *Hub) trackVoice(s *session, ch int64) {
	h.mu.Lock()
	if s.vsCh == ch {
		h.mu.Unlock()
		return
	}
	oldID := s.vsID
	s.vsID, s.vsCh = 0, ch
	h.mu.Unlock()

	ctx := context.Background()
	st := h.opts.Store
	now := time.Now().UnixMilli()
	if oldID != 0 {
		if err := st.VoiceEnd(ctx, oldID, now); err != nil {
			log.Printf("статистика: %v", err)
		}
	}
	if ch == 0 || s.nickname == "" {
		return
	}
	name := ""
	if c, err := st.Channel(ctx, ch); err == nil {
		name = c.Name
	}
	id, err := st.VoiceStart(ctx, s.nickname, ch, name, now)
	if err != nil {
		log.Printf("статистика: %v", err)
		return
	}
	h.mu.Lock()
	ok := s.vsCh == ch && s.vsID == 0
	if ok {
		s.vsID = id
	}
	h.mu.Unlock()
	if !ok { // пока писали, человек уже ушёл или перешёл
		_ = st.VoiceEnd(ctx, id, now)
	}
}

// activeVoice — id идущих сессий.
func (h *Hub) activeVoice() map[int64]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := map[int64]bool{}
	for _, s := range h.sessions {
		if s.vsID != 0 {
			ids[s.vsID] = true
		}
	}
	return ids
}

func (h *Hub) voiceTouchLoop() {
	t := time.NewTicker(voiceTouchEvery)
	defer t.Stop()
	for range t.C {
		now := time.Now().UnixMilli()
		for id := range h.activeVoice() {
			_ = h.opts.Store.VoiceTouch(context.Background(), id, now)
		}
	}
}

// sendVoiceStats — «voice-stats» {ttl: за сколько секунд, 0 — за всё время}. Видят все участники.
func (h *Hub) sendVoiceStats(s *session, ttl int64) {
	now := time.Now().UnixMilli()
	var since int64
	if ttl > 0 {
		since = now - ttl*1000
	}
	st, err := h.opts.Store.VoiceStats(context.Background(), since, now, h.activeVoice())
	if err != nil {
		log.Printf("статистика: %v", err)
		s.sendError("Не удалось посчитать статистику.")
		return
	}
	s.send("voice-stats", map[string]any{"ttl": ttl, "stats": st})
}
