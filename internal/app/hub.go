// Package app — WebSocket-протокол: сессии, присутствие, чат, голос (через SFU) и админка.
package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"zalupa228local/internal/sfu"
	"zalupa228local/internal/store"
)

type Options struct {
	Store          *store.Store
	SFU            *sfu.SFU
	ServerName     string
	ServerSlogan   string
	ServerPassword string
	AdminPassword  string
	UploadDir      string
	MaxUploadBytes int64
}

type Hub struct {
	opts Options

	mu       sync.Mutex
	sessions map[string]*session // по id сессии
	byNick   map[string]*session // по нику в нижнем регистре
	mutedBy  map[string]bool     // ники, заглушённые админом (до перезапуска сервера)
	guard    *guard              // защита от перебора ключа и пароля админа
}

func NewHub(opts Options) *Hub {
	return &Hub{opts: opts, sessions: map[string]*session{}, byNick: map[string]*session{}, mutedBy: map[string]bool{}, guard: newGuard()}
}

type session struct {
	hub      *Hub
	id       string
	clientID string
	nickname string
	conn     *websocket.Conn
	out      chan []byte
	done     chan struct{}
	closing  chan struct{} // закрыть соединение, дописав очередь (отказ во входе, кик)
	closeReq sync.Once
	stopOnce sync.Once

	// под hub.mu
	admin        bool
	voiceChannel int64
	muted        bool
	deafened     bool
	screen       bool
	camera       bool
	serverMuted  bool    // заглушён админом (держится до переподключения ника)
	qRTT         int     // качество связи с сервером по словам клиента: пинг, мс (-1 — нет данных)
	qLoss        float64 // потери пакетов, %
	qJitter      int
	qAt          time.Time
	peer         *sfu.Peer
	joinID       string // id текущего входа в голос; сигналинг со старым id игнорируется

	// антиспам (только из read-горутины)
	bucket     float64
	bucketTime time.Time

	ip         string
	inviteCode string // по какому ключу-приглашению вошёл ("" — общий ключ или сервер открыт)
}

type inMsg struct {
	Type        string                   `json:"type"`
	Nickname    string                   `json:"nickname"`
	ClientID    string                   `json:"clientId"`
	Password    string                   `json:"password"`
	AdminToken  string                   `json:"adminToken"`
	ChannelID   int64                    `json:"channelId"`
	Before      int64                    `json:"before"`
	ID          int64                    `json:"id"`
	Text        string                   `json:"text"`
	ReplyTo     int64                    `json:"replyTo"`
	Attachments []store.Attachment       `json:"attachments"`
	Emoji       string                   `json:"emoji"`
	Nonce       string                   `json:"nonce"`
	Kind        string                   `json:"kind"`
	Name        string                   `json:"name"`
	Dir         int                      `json:"dir"`
	SessionID   string                   `json:"sessionId"`
	JoinID      string                   `json:"joinId"`
	Rtt         *float64                 `json:"rtt"`
	Loss        *float64                 `json:"loss"`
	Jitter      *float64                 `json:"jitter"`
	Muted       *bool                    `json:"muted"`
	Deafened    *bool                    `json:"deafened"`
	Screen      *bool                    `json:"screen"`
	Camera      *bool                    `json:"camera"`
	Value       bool                     `json:"value"`
	SDP         string                   `json:"sdp"`
	Candidate   *webrtc.ICECandidateInit `json:"candidate"`
	T           float64                  `json:"t"`
}

type userView struct {
	ID           string   `json:"id"`
	Nickname     string   `json:"nickname"`
	VoiceChannel int64    `json:"voiceChannelId"`
	Muted        bool     `json:"muted"`
	Deafened     bool     `json:"deafened"`
	Screen       bool     `json:"screen"`
	Camera       bool     `json:"camera"`
	Admin        bool     `json:"admin"`
	ServerMuted  bool     `json:"serverMuted"`
	Quality      *quality `json:"quality,omitempty"`
}

type quality struct {
	RTT    int     `json:"rtt"`
	Loss   float64 `json:"loss"`
	Jitter int     `json:"jitter"`
}

func (s *session) qualityLocked() *quality {
	if s.voiceChannel == 0 || s.qAt.IsZero() || time.Since(s.qAt) > 30*time.Second {
		return nil
	}
	return &quality{RTT: s.qRTT, Loss: s.qLoss, Jitter: s.qJitter}
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

const (
	pingEvery   = 25 * time.Second
	readTimeout = 70 * time.Second
	maxText     = 4000
)

func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s := &session{hub: h, conn: conn, out: make(chan []byte, 512), done: make(chan struct{}), closing: make(chan struct{}), ip: clientIP(r)}
	go s.writeLoop()
	s.readLoop()
}

func (s *session) writeLoop() {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case b := <-s.out:
			_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := s.conn.WriteMessage(websocket.TextMessage, b); err != nil {
				s.stop()
				return
			}
		case <-t.C:
			if err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				s.stop()
				return
			}
		case <-s.closing:
			// дописываем всё, что успели поставить в очередь (например, причину отказа), и закрываемся
			for {
				select {
				case b := <-s.out:
					_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
					if err := s.conn.WriteMessage(websocket.TextMessage, b); err != nil {
						s.stop()
						return
					}
					continue
				default:
				}
				break
			}
			_ = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(2*time.Second))
			s.stop()
			return
		}
	}
}

// closeAfterFlush закрывает соединение после отправки уже поставленных в очередь сообщений и ждёт этого.
func (s *session) closeAfterFlush() {
	s.requestClose()
	<-s.done
}

// requestClose — то же без ожидания; если клиент не читает, через 3 с рвём соединение.
func (s *session) requestClose() {
	s.closeReq.Do(func() {
		close(s.closing)
		go func() {
			select {
			case <-s.done:
			case <-time.After(3 * time.Second):
				s.stop()
			}
		}()
	})
}

func (s *session) stop() {
	s.stopOnce.Do(func() {
		close(s.done)
		_ = s.conn.Close()
	})
}

func (s *session) sendRaw(b []byte) {
	select {
	case <-s.done:
	case s.out <- b:
	default:
		log.Printf("ws %s: клиент не успевает читать, отключаем", s.nickname)
		s.stop()
	}
}

func (s *session) send(typ string, payload map[string]any) {
	s.sendRaw(encode(typ, payload))
}

func (s *session) sendError(msg string) {
	s.send("error", map[string]any{"message": msg})
}

func encode(typ string, payload map[string]any) []byte {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["type"] = typ
	b, _ := json.Marshal(payload)
	return b
}

func (s *session) readLoop() {
	defer func() {
		s.stop()
		s.hub.unregister(s)
	}()
	s.conn.SetReadLimit(1 << 20)
	_ = s.conn.SetReadDeadline(time.Now().Add(readTimeout))
	s.conn.SetPongHandler(func(string) error {
		return s.conn.SetReadDeadline(time.Now().Add(readTimeout))
	})

	registered := false
	for {
		_, raw, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(readTimeout))
		var m inMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			s.sendError("Некорректный JSON.")
			continue
		}
		if !registered {
			if m.Type != "hello" {
				s.sendError("Сначала hello.")
				continue
			}
			if !s.hub.register(s, &m) {
				s.closeAfterFlush()
				return
			}
			registered = true
			continue
		}
		s.hub.handle(s, &m)
	}
}

// ---------- регистрация ----------

var nickBad = regexp.MustCompile(`[\x00-\x1f\x7f<>]`)

func normalizeNick(n string) (string, bool) {
	n = strings.Join(strings.Fields(n), " ")
	if n == "" || len([]rune(n)) > 32 || nickBad.MatchString(n) {
		return "", false
	}
	return n, true
}

func (h *Hub) register(s *session, m *inMsg) bool {
	ctx := context.Background()
	inviteCode := ""
	adminByToken := h.opts.AdminPassword != "" && hmac.Equal([]byte(m.AdminToken), []byte(h.adminToken(strings.TrimSpace(m.ClientID))))
	// админ (валидный токен этого браузера) входит без ключа — чтобы не запереть себя снаружи
	if !adminByToken && h.accessRequired(ctx) {
		if h.guard.blocked(s.ip) {
			s.send("hello-rejected", map[string]any{"reason": "key", "message": "Слишком много неверных попыток. Попробуйте через 15 минут."})
			return false
		}
		code, ok := h.checkKey(ctx, m.Password)
		if !ok {
			msg := "Неверный ключ доступа."
			if strings.TrimSpace(m.Password) == "" {
				msg = "Сервер закрыт: нужен ключ доступа. Попросите у админа ссылку-приглашение."
			} else if h.guard.fail(s.ip) {
				log.Printf("отказ входа (%s): перебор ключа — IP заблокирован на 15 минут", s.ip)
			} else {
				log.Printf("отказ входа (%s): неверный ключ доступа", s.ip)
			}
			s.send("hello-rejected", map[string]any{"reason": "key", "message": msg})
			return false
		}
		inviteCode = code
	}
	nick, ok := normalizeNick(m.Nickname)
	if !ok {
		log.Printf("отказ входа (%s): плохой ник %q", s.conn.RemoteAddr(), m.Nickname)
		s.send("hello-rejected", map[string]any{"reason": "nickname", "message": "Ник: от 1 до 32 символов, без < >."})
		return false
	}
	clientID := strings.TrimSpace(m.ClientID)
	if len(clientID) < 8 || len(clientID) > 64 {
		s.send("hello-rejected", map[string]any{"reason": "client", "message": "Некорректный clientId."})
		return false
	}

	key := strings.ToLower(nick)
	h.mu.Lock()
	old := h.byNick[key]
	if old != nil && old.clientID != clientID {
		h.mu.Unlock()
		log.Printf("отказ входа (%s): ник %q уже занят", s.conn.RemoteAddr(), nick)
		s.send("hello-rejected", map[string]any{"reason": "nickname-taken", "message": "Этот ник уже занят."})
		return false
	}
	s.id = randomHex(12)
	s.inviteCode = inviteCode
	s.clientID = clientID
	s.nickname = nick
	s.admin = h.opts.AdminPassword != "" && hmac.Equal([]byte(m.AdminToken), []byte(h.adminToken(clientID)))
	s.serverMuted = h.mutedBy[key]
	h.sessions[s.id] = s
	h.byNick[key] = s
	h.mu.Unlock()

	if old != nil {
		// Тот же браузер переподключился (обновил вкладку/открыл вторую) — старую сессию закрываем.
		log.Printf("%s: вход из другой вкладки/окна того же браузера — старая сессия закрыта", nick)
		old.send("replaced", map[string]any{"message": "Вы подключились из другой вкладки."})
		h.dropSession(old)
	}

	channels, err := h.opts.Store.Channels(context.Background())
	if err != nil {
		log.Printf("channels: %v", err)
	}
	avatars, err := h.opts.Store.Avatars(context.Background())
	if err != nil {
		log.Printf("avatars: %v", err)
	}
	h.mu.Lock()
	users := h.usersLocked()
	isAdmin := s.admin
	h.mu.Unlock()
	s.send("ready", map[string]any{
		"sessionId":      s.id,
		"nickname":       nick,
		"admin":          isAdmin,
		"server":         map[string]any{"name": h.opts.ServerName, "slogan": h.opts.ServerSlogan},
		"channels":       channels,
		"users":          users,
		"maxUploadBytes": h.opts.MaxUploadBytes,
		"avatars":        avatars,
	})
	log.Printf("+ %s", nick)
	h.broadcastPresence()
	return true
}

func (h *Hub) dropSession(s *session) {
	h.mu.Lock()
	h.removeLocked(s)
	peer := s.peer
	s.peer = nil
	s.voiceChannel = 0
	h.mu.Unlock()
	if peer != nil {
		peer.Close()
	}
	s.requestClose()
}

func (h *Hub) removeLocked(s *session) bool {
	if h.sessions[s.id] != s {
		return false
	}
	delete(h.sessions, s.id)
	if h.byNick[strings.ToLower(s.nickname)] == s {
		delete(h.byNick, strings.ToLower(s.nickname))
	}
	return true
}

func (h *Hub) unregister(s *session) {
	h.mu.Lock()
	removed := h.removeLocked(s)
	peer := s.peer
	s.peer = nil
	s.voiceChannel = 0
	h.mu.Unlock()
	if peer != nil {
		peer.Close()
	}
	if removed {
		log.Printf("- %s", s.nickname)
		h.broadcastPresence()
	}
}

func (h *Hub) adminToken(clientID string) string {
	mac := hmac.New(sha256.New, []byte(h.opts.AdminPassword))
	mac.Write([]byte("z228-admin:" + clientID))
	return hex.EncodeToString(mac.Sum(nil))
}

func (h *Hub) usersLocked() []userView {
	out := make([]userView, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, userView{
			ID: s.id, Nickname: s.nickname, VoiceChannel: s.voiceChannel,
			Muted: s.muted, Deafened: s.deafened, Screen: s.screen, Camera: s.camera, Admin: s.admin, ServerMuted: s.serverMuted, Quality: s.qualityLocked(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Nickname) < strings.ToLower(out[j].Nickname) })
	return out
}

func (h *Hub) broadcast(typ string, payload map[string]any, except *session) {
	b := encode(typ, payload)
	h.mu.Lock()
	list := make([]*session, 0, len(h.sessions))
	for _, s := range h.sessions {
		if s != except {
			list = append(list, s)
		}
	}
	h.mu.Unlock()
	for _, s := range list {
		s.sendRaw(b)
	}
}

func (h *Hub) broadcastPresence() {
	h.mu.Lock()
	users := h.usersLocked()
	h.mu.Unlock()
	h.broadcast("presence", map[string]any{"users": users}, nil)
}

func (h *Hub) broadcastChannels() {
	channels, err := h.opts.Store.Channels(context.Background())
	if err != nil {
		log.Printf("channels: %v", err)
		return
	}
	h.broadcast("channels", map[string]any{"channels": channels}, nil)
}

// ---------- обработка сообщений ----------

func (s *session) allow(cost float64) bool {
	const rate, burst = 2.0, 12.0 // 2 действия в секунду, всплеск до 12
	now := time.Now()
	if s.bucketTime.IsZero() {
		s.bucket = burst
	} else {
		s.bucket = min(burst, s.bucket+now.Sub(s.bucketTime).Seconds()*rate)
	}
	s.bucketTime = now
	if s.bucket < cost {
		return false
	}
	s.bucket -= cost
	return true
}

func (h *Hub) handle(s *session, m *inMsg) {
	ctx := context.Background()
	st := h.opts.Store

	switch m.Type {
	case "ping":
		s.send("pong", map[string]any{"t": m.T})

	case "history":
		ch, err := st.Channel(ctx, m.ChannelID)
		if err != nil || ch.Kind != "text" {
			s.sendError("Канал не найден.")
			return
		}
		msgs, more, err := st.History(ctx, m.ChannelID, m.Before, 50)
		if err != nil {
			log.Printf("history: %v", err)
			s.sendError("Не удалось загрузить историю.")
			return
		}
		s.send("history", map[string]any{"channelId": m.ChannelID, "before": m.Before, "messages": msgs, "hasMore": more})

	case "send":
		if !s.allow(1) {
			s.sendError("Слишком часто, подождите немного.")
			return
		}
		ch, err := st.Channel(ctx, m.ChannelID)
		if err != nil || ch.Kind != "text" {
			s.sendError("Канал не найден.")
			return
		}
		text := clampText(m.Text)
		atts := h.validAttachments(m.Attachments)
		if strings.TrimSpace(text) == "" && len(atts) == 0 {
			return
		}
		msg, err := st.CreateMessage(ctx, m.ChannelID, s.nickname, text, m.ReplyTo, atts)
		if err != nil {
			log.Printf("send: %v", err)
			s.sendError("Не удалось отправить сообщение.")
			return
		}
		h.broadcast("message", map[string]any{"message": msg, "nonce": m.Nonce, "from": s.id}, nil)

	case "edit":
		msg, err := st.EditMessage(ctx, m.ID, s.nickname, clampText(m.Text))
		if err != nil {
			s.sendError(storeErr(err, "Не удалось изменить сообщение."))
			return
		}
		h.broadcast("message-update", map[string]any{"message": msg}, nil)

	case "delete":
		h.mu.Lock()
		admin := s.admin
		h.mu.Unlock()
		msg, err := st.DeleteMessage(ctx, m.ID, s.nickname, admin)
		if err != nil {
			s.sendError(storeErr(err, "Не удалось удалить сообщение."))
			return
		}
		h.broadcast("message-delete", map[string]any{"id": msg.ID, "channelId": msg.ChannelID}, nil)

	case "react":
		emoji := strings.TrimSpace(m.Emoji)
		if emoji == "" || len(emoji) > 32 || !s.allow(0.5) {
			return
		}
		msg, err := st.ToggleReaction(ctx, m.ID, emoji, s.nickname)
		if err != nil {
			s.sendError(storeErr(err, "Не удалось поставить реакцию."))
			return
		}
		h.broadcast("message-update", map[string]any{"message": msg}, nil)

	case "set-avatar":
		if !s.allow(2) {
			s.sendError("Слишком часто, подождите немного.")
			return
		}
		url := strings.TrimSpace(m.Text)
		if url != "" {
			ok := uploadURL.MatchString(url) && strings.HasPrefix(mimeForExt(filepath.Ext(url)), "image/")
			if ok {
				_, err := os.Stat(filepath.Join(h.opts.UploadDir, filepath.Base(url)))
				ok = err == nil
			}
			if !ok {
				s.sendError("Аватарка должна быть картинкой (PNG, JPG, GIF, WebP).")
				return
			}
		}
		if err := st.SetAvatar(ctx, s.nickname, url); err != nil {
			log.Printf("avatar: %v", err)
			s.sendError("Не удалось сохранить аватарку.")
			return
		}
		h.broadcast("avatar", map[string]any{"nickname": strings.ToLower(s.nickname), "url": url}, nil)

	case "client-diag":
		// браузер сообщает, почему у него не запускается голос (UA, наличие WebRTC и т.п.)
		if s.allow(3) {
			d := m.Text
			if len(d) > 2000 {
				d = d[:2000]
			}
			log.Printf("диагностика %s: %s", s.nickname, d)
		}

	case "voice-quality":
		// клиент раз в несколько секунд сообщает пинг и потери до сервера — показываем всем
		h.mu.Lock()
		if s.voiceChannel == 0 || time.Since(s.qAt) < 2*time.Second || m.Rtt == nil {
			h.mu.Unlock()
			return
		}
		s.qRTT = clampInt(int(*m.Rtt), 0, 10000)
		s.qLoss = 0
		if m.Loss != nil {
			s.qLoss = float64(clampInt(int(*m.Loss*10), 0, 1000)) / 10
		}
		s.qJitter = 0
		if m.Jitter != nil {
			s.qJitter = clampInt(int(*m.Jitter), 0, 10000)
		}
		s.qAt = time.Now()
		q := s.qualityLocked()
		h.mu.Unlock()
		h.broadcast("quality", map[string]any{"userId": s.id, "quality": q}, nil)

	case "typing":
		h.broadcast("typing", map[string]any{"channelId": m.ChannelID, "userId": s.id, "nickname": s.nickname}, s)

	case "voice-join":
		h.voiceJoin(s, m.ChannelID, m.JoinID)

	case "voice-leave":
		h.mu.Lock()
		peer := s.peer
		h.mu.Unlock()
		if peer != nil {
			peer.Close() // onClosed обновит присутствие
		}

	case "voice-state":
		h.mu.Lock()
		if m.Muted != nil {
			s.muted = *m.Muted
		}
		if m.Deafened != nil {
			s.deafened = *m.Deafened
		}
		if m.Screen != nil {
			s.screen = *m.Screen && s.voiceChannel != 0
		}
		if m.Camera != nil {
			s.camera = *m.Camera && s.voiceChannel != 0
		}
		h.mu.Unlock()
		h.broadcastPresence()

	case "rtc-answer":
		h.mu.Lock()
		peer := s.peer
		if m.JoinID != s.joinID {
			peer = nil // ответ от прошлого входа в голос
		}
		h.mu.Unlock()
		if peer != nil {
			if err := peer.HandleAnswer(m.SDP); err != nil {
				log.Printf("RTC %s: answer: %v", s.nickname, err)
			}
		}

	case "rtc-candidate":
		h.mu.Lock()
		peer := s.peer
		if m.JoinID != s.joinID {
			peer = nil
		}
		h.mu.Unlock()
		if peer != nil && m.Candidate != nil {
			if err := peer.HandleCandidate(*m.Candidate); err != nil {
				log.Printf("RTC %s: candidate: %v", s.nickname, err)
			}
		}

	case "admin-login":
		if h.guard.blocked(s.ip) {
			s.send("admin", map[string]any{"ok": false, "message": "Слишком много неверных попыток. Попробуйте через 15 минут."})
			return
		}
		if h.opts.AdminPassword == "" || subtle.ConstantTimeCompare([]byte(m.Password), []byte(h.opts.AdminPassword)) != 1 {
			if h.guard.fail(s.ip) {
				log.Printf("%s (%s): перебор пароля админа — IP заблокирован на 15 минут", s.nickname, s.ip)
			}
			time.Sleep(500 * time.Millisecond)
			s.send("admin", map[string]any{"ok": false, "message": "Неверный пароль."})
			return
		}
		h.mu.Lock()
		s.admin = true
		h.mu.Unlock()
		s.send("admin", map[string]any{"ok": true, "token": h.adminToken(s.clientID)})
		h.broadcastPresence()

	case "admin-logout":
		h.mu.Lock()
		s.admin = false
		h.mu.Unlock()
		s.send("admin", map[string]any{"ok": false})
		h.broadcastPresence()

	case "channel-create", "channel-rename", "channel-delete", "channel-move", "kick", "disconnect-voice", "server-mute",
		"invites", "invite-create", "invite-delete":
		h.mu.Lock()
		admin := s.admin
		h.mu.Unlock()
		if !admin {
			s.sendError("Нужны права администратора.")
			return
		}
		if strings.HasPrefix(m.Type, "invite") {
			h.handleInvites(s, m)
			return
		}
		h.handleAdmin(s, m)

	default:
		s.sendError("Неизвестная команда: " + m.Type)
	}
}

func (h *Hub) voiceJoin(s *session, channelID int64, joinID string) {
	ch, err := h.opts.Store.Channel(context.Background(), channelID)
	if err != nil || ch.Kind != "voice" {
		s.sendError("Голосовой канал не найден.")
		return
	}
	// Пустой joinId шлют страницы старой версии (до F5) — для них проверка просто совпадает с "".
	if len(joinID) > 64 {
		joinID = joinID[:64]
	}
	h.mu.Lock()
	old := s.peer
	s.peer = nil
	s.joinID = joinID
	h.mu.Unlock()
	if old != nil {
		old.Close()
	}

	var peer *sfu.Peer
	onClosed := func() {
		h.mu.Lock()
		changed := s.peer == peer && peer != nil
		if changed {
			s.peer = nil
			s.qAt = time.Time{}
			s.voiceChannel = 0
			s.screen = false
			s.camera = false
		}
		h.mu.Unlock()
		if changed {
			s.send("voice-left", map[string]any{"joinId": joinID})
			h.broadcastPresence()
		}
	}
	// Все сообщения этого соединения помечаем joinId, чтобы клиент отбросил хвосты прошлых входов.
	signal := func(typ string, payload map[string]any) {
		if payload == nil {
			payload = map[string]any{}
		}
		payload["joinId"] = joinID
		s.send(typ, payload)
	}
	// Сигнал «voice-joined» должен уйти раньше первого offer.
	signal("voice-joined", map[string]any{"channelId": channelID})
	peer, err = h.opts.SFU.Join(strconv.FormatInt(channelID, 10), s.id, signal, onClosed)
	if err != nil {
		log.Printf("RTC join: %v", err)
		signal("voice-left", map[string]any{"message": "Не удалось подключиться к голосу."})
		return
	}
	h.mu.Lock()
	peer.SetServerMuted(s.serverMuted)
	s.peer = peer
	s.voiceChannel = channelID
	s.screen = false
	s.camera = false
	h.mu.Unlock()
	h.broadcastPresence()
}

func (h *Hub) handleAdmin(s *session, m *inMsg) {
	ctx := context.Background()
	st := h.opts.Store
	switch m.Type {
	case "channel-create":
		name := channelName(m.Name, m.Kind)
		if name == "" || (m.Kind != "text" && m.Kind != "voice") {
			s.sendError("Укажите тип и название канала.")
			return
		}
		if _, err := st.CreateChannel(ctx, m.Kind, name); err != nil {
			s.sendError("Не удалось создать канал.")
			return
		}
	case "channel-rename":
		ch, err := st.Channel(ctx, m.ID)
		if err != nil {
			s.sendError("Канал не найден.")
			return
		}
		name := channelName(m.Name, ch.Kind)
		if name == "" {
			s.sendError("Пустое название.")
			return
		}
		if err := st.RenameChannel(ctx, m.ID, name); err != nil {
			s.sendError("Не удалось переименовать.")
			return
		}
	case "channel-move":
		if err := st.MoveChannel(ctx, m.ID, m.Dir); err != nil {
			s.sendError("Не удалось переместить.")
			return
		}
	case "channel-delete":
		chs, _ := st.Channels(ctx)
		var target store.Channel
		same := 0
		for _, c := range chs {
			if c.ID == m.ID {
				target = c
			}
		}
		for _, c := range chs {
			if c.Kind == target.Kind {
				same++
			}
		}
		if target.ID == 0 {
			s.sendError("Канал не найден.")
			return
		}
		if same <= 1 {
			s.sendError("Нельзя удалить последний канал такого типа.")
			return
		}
		if err := st.DeleteChannel(ctx, m.ID); err != nil {
			s.sendError("Не удалось удалить.")
			return
		}
		if target.Kind == "voice" {
			for _, p := range h.peersInVoice(target.ID) {
				p.Close()
			}
		}
	case "kick":
		h.mu.Lock()
		target := h.sessions[m.SessionID]
		h.mu.Unlock()
		if target == nil {
			s.sendError("Пользователь не в сети.")
			return
		}
		target.send("kicked", map[string]any{"message": "Вас отключил администратор " + s.nickname + "."})
		go func() {
			time.Sleep(200 * time.Millisecond)
			h.dropSession(target)
			h.broadcastPresence()
		}()
		return
	case "server-mute":
		h.mu.Lock()
		target := h.sessions[m.SessionID]
		var peer *sfu.Peer
		if target != nil {
			target.serverMuted = m.Value
			peer = target.peer
			if m.Value {
				h.mutedBy[strings.ToLower(target.nickname)] = true
			} else {
				delete(h.mutedBy, strings.ToLower(target.nickname))
			}
		}
		h.mu.Unlock()
		if target == nil {
			s.sendError("Пользователь не в сети.")
			return
		}
		if peer != nil {
			peer.SetServerMuted(m.Value)
		}
		h.broadcastPresence()
		return
	case "disconnect-voice":
		h.mu.Lock()
		target := h.sessions[m.SessionID]
		var peer *sfu.Peer
		if target != nil {
			peer = target.peer
		}
		h.mu.Unlock()
		if peer != nil {
			peer.Close()
		}
		return
	}
	h.broadcastChannels()
}

func (h *Hub) peersInVoice(channelID int64) []*sfu.Peer {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*sfu.Peer
	for _, s := range h.sessions {
		if s.voiceChannel == channelID && s.peer != nil {
			out = append(out, s.peer)
		}
	}
	return out
}

// ---------- вспомогательное ----------

func clampText(t string) string {
	t = strings.TrimRightFunc(t, unicode.IsSpace)
	t = strings.TrimLeft(t, "\r\n")
	if r := []rune(t); len(r) > maxText {
		t = string(r[:maxText])
	}
	return t
}

func channelName(name, _ string) string {
	name = strings.Join(strings.Fields(name), " ")
	if r := []rune(name); len(r) > 50 {
		name = string(r[:50])
	}
	return name
}

var uploadURL = regexp.MustCompile(`^/uploads/[a-f0-9]{24}(\.[a-z0-9]{1,8})?$`)

func (h *Hub) validAttachments(in []store.Attachment) []store.Attachment {
	out := []store.Attachment{}
	for _, a := range in {
		if len(out) >= 10 {
			break
		}
		if !uploadURL.MatchString(a.URL) {
			continue
		}
		fi, err := os.Stat(filepath.Join(h.opts.UploadDir, filepath.Base(a.URL)))
		if err != nil || fi.IsDir() {
			continue
		}
		name := filepath.Base(strings.ReplaceAll(a.Name, "\\", "/"))
		if r := []rune(name); len(r) > 120 {
			name = string(r[:120])
		}
		if name == "" || name == "." {
			name = filepath.Base(a.URL)
		}
		out = append(out, store.Attachment{
			URL:    a.URL,
			Name:   name,
			Mime:   mimeForExt(filepath.Ext(a.URL)),
			Size:   fi.Size(),
			Width:  clampInt(a.Width, 0, 20000),
			Height: clampInt(a.Height, 0, 20000),
		})
	}
	return out
}

func clampInt(v, lo, hi int) int {
	return max(lo, min(hi, v))
}

func storeErr(err error, def string) string {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "Сообщение не найдено."
	case errors.Is(err, store.ErrForbidden):
		return "Это не ваше сообщение."
	default:
		return def
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SessionNick — ник по id сессии (для HTTP-загрузок).
func (h *Hub) SessionNick(id string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sessions[id]
	if s == nil {
		return "", false
	}
	return s.nickname, true
}
