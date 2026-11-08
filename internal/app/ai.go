package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"zalupa228local/internal/store"
)

// ИИ в чате: DeepSeek отвечает на @ai и /ai тем, у кого есть право «Звать ИИ».
// Ключ API хранится в настройках сервера и наружу не отдаётся.

const (
	botNick      = "AI"
	aiURL        = "https://api.deepseek.com/chat/completions"
	aiModel      = "deepseek-chat"
	aiHistory    = 14 // сколько последних сообщений канала даём как контекст
	aiCooldown   = 3 * time.Second
	aiParallel   = 2
	aiTimeout    = 45 * time.Second
	aiMaxTokens  = 350
	aiSettingKey = "ai_key"
	aiSettingOn  = "ai_enabled"
)

const aiPrompt = `Ты — AI, бот в чате компании друзей на сервере «%s» (голосовой чат для игр, как Discord).
Отвечай по-русски и КОРОТКО: обычно 1–3 предложения, максимум ~350 символов, если прямо не просят подробнее.
Сначала суть и по делу, потом можно подколоть или пошутить. Стиль — дружеский, дерзкий, смешной, можно сленг и лёгкий мат, но без кринжа и без оскорблений по-настоящему.
Без заголовков, без длинных списков и без вступлений вроде «Отличный вопрос». Код — только если просят, и коротко.
В истории каждое сообщение подписано ником автора: «Ник: текст». Тебя зовут через @ai или /ai.`

var aiMention = regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}_])@ai($|[^\p{L}\p{N}_])`)

// isAICall — сообщение зовёт ИИ: начинается с /ai или содержит @ai.
func isAICall(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if strings.HasPrefix(t, "/ai") && (len(t) == 3 || t[3] == ' ' || t[3] == '\n') {
		return true
	}
	return aiMention.MatchString(text)
}

type aiState struct {
	mu   sync.Mutex
	last map[string]time.Time
	sem  chan struct{}
}

func newAIState() *aiState {
	return &aiState{last: map[string]time.Time{}, sem: make(chan struct{}, aiParallel)}
}

func (h *Hub) aiConfig(ctx context.Context) (key string, enabled bool) {
	key, _, _ = h.opts.Store.GetSetting(ctx, aiSettingKey)
	on, ok, _ := h.opts.Store.GetSetting(ctx, aiSettingOn)
	return key, !ok || on != "0"
}

// maybeAI вызывается после каждого нового сообщения.
func (h *Hub) maybeAI(s *session, msg store.Message, dmMembers []string) {
	if !isAICall(msg.Text) {
		return
	}
	if !h.can(s, PermAI) {
		s.sendError("ИИ отвечает только тем, у кого роль с правом «Звать ИИ».")
		return
	}
	key, enabled := h.aiConfig(context.Background())
	if key == "" || !enabled {
		s.sendError("ИИ не настроен: админ должен вставить ключ DeepSeek в «Администрирование → ИИ».")
		return
	}
	nick := strings.ToLower(s.nickname)
	h.ai.mu.Lock()
	if time.Since(h.ai.last[nick]) < aiCooldown {
		h.ai.mu.Unlock()
		s.sendError("Не так часто — ИИ ещё не отдышался.")
		return
	}
	h.ai.last[nick] = time.Now()
	h.ai.mu.Unlock()
	select {
	case h.ai.sem <- struct{}{}:
	default:
		s.sendError("ИИ занят, попробуйте через пару секунд.")
		return
	}
	go func() {
		defer func() { <-h.ai.sem }()
		h.aiReply(key, msg, dmMembers)
	}()
}

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (h *Hub) aiReply(key string, q store.Message, dmMembers []string) {
	ctx, cancel := context.WithTimeout(context.Background(), aiTimeout)
	defer cancel()

	// «печатает…», пока ждём ответ (у клиента подсказка гаснет через 6 с)
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(4 * time.Second)
		defer t.Stop()
		for {
			h.broadcastIn(dmMembers, "typing", map[string]any{"channelId": q.ChannelID, "userId": "ai", "nickname": botNick}, nil)
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()

	msgs := []chatMsg{{Role: "system", Content: fmt.Sprintf(aiPrompt, h.opts.ServerName)}}
	hist, _, err := h.opts.Store.History(ctx, q.ChannelID, q.ID+1, aiHistory)
	if err != nil || len(hist) == 0 {
		hist = []store.Message{q}
	}
	for _, m := range hist {
		text := strings.TrimSpace(m.Text)
		if len(m.Attachments) > 0 {
			text += " [вложение]"
		}
		if m.Author == botNick {
			msgs = append(msgs, chatMsg{Role: "assistant", Content: text})
			continue
		}
		if lt := strings.ToLower(text); strings.HasPrefix(lt, "/ai") {
			text = strings.TrimSpace(text[3:])
		}
		msgs = append(msgs, chatMsg{Role: "user", Content: m.Author + ": " + text})
	}

	answer, err := callDeepSeek(ctx, key, msgs)
	close(stop)
	if err != nil {
		log.Printf("ИИ: %v", err)
		answer = "Чёт я завис 🤖 Попробуйте ещё раз чуть позже."
	}
	answer = clampText(answer)
	if answer == "" {
		answer = "…"
	}
	out, err := h.opts.Store.CreateMessage(context.Background(), q.ChannelID, botNick, answer, q.ID, nil)
	if err != nil {
		log.Printf("ИИ: сохранить ответ: %v", err)
		return
	}
	h.broadcastIn(dmMembers, "message", map[string]any{"message": out, "from": "ai"}, nil)
}

var aiClient = &http.Client{Timeout: aiTimeout}

func callDeepSeek(ctx context.Context, key string, msgs []chatMsg) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":       aiModel,
		"messages":    msgs,
		"max_tokens":  aiMaxTokens,
		"temperature": 1.3,
		"stream":      false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, aiURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := aiClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", fmt.Errorf("DeepSeek %d: %s", resp.StatusCode, msg)
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	if len(r.Choices) == 0 {
		return "", errors.New("пустой ответ")
	}
	return strings.TrimSpace(r.Choices[0].Message.Content), nil
}

// handleAIConfig — настройки ИИ в админке: ai-settings (прочитать), ai-config {text: ключ, value: включён, kind:"clear"}.
func (h *Hub) handleAIConfig(s *session, m *inMsg) {
	ctx := context.Background()
	st := h.opts.Store
	if m.Type == "ai-config" {
		if m.Kind == "clear" {
			_ = st.SetSetting(ctx, aiSettingKey, "")
		} else if k := strings.TrimSpace(m.Text); k != "" {
			if err := st.SetSetting(ctx, aiSettingKey, k); err != nil {
				s.sendError("Не удалось сохранить ключ.")
				return
			}
		}
		on := "0"
		if m.Value {
			on = "1"
		}
		_ = st.SetSetting(ctx, aiSettingOn, on)
		log.Printf("%s изменил настройки ИИ", s.nickname)
	}
	key, enabled := h.aiConfig(ctx)
	s.send("ai-settings", map[string]any{"configured": key != "", "enabled": enabled})
}
