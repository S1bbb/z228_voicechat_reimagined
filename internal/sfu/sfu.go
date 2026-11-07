// Package sfu — Selective Forwarding Unit на pion/webrtc.
//
// Каждый участник голосового канала держит ОДНО соединение с сервером. Сервер принимает его
// микрофон (и демонстрацию экрана) и пересылает остальным участникам того же канала.
// Всё медиа идёт через один UDP-порт (и тот же TCP-порт как запасной вариант),
// поэтому клиентам не нужен P2P, TURN или Radmin — достаточно, чтобы был доступен сервер.
//
// Переговоры всегда начинает сервер: он шлёт offer, клиент отвечает answer.
// Первые три m-line — входящие от клиента: 0 — микрофон, 1 — демонстрация экрана, 2 — камера;
// остальные — пересылаемые треки других участников.
package sfu

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

type Config struct {
	Port int
	// PublicIPs возвращает дополнительные адреса (внешний IP, IP Radmin/Hamachi и т.п.),
	// которые сообщаются клиентам как кандидаты в дополнение к адресам интерфейсов.
	PublicIPs func() []string
}

// Signal — отправка сообщения клиенту через WebSocket.
type Signal func(typ string, payload map[string]any)

type SFU struct {
	cfg Config
	api *webrtc.API

	mu    sync.Mutex
	rooms map[string]*room
}

func New(cfg Config) (*SFU, error) {
	se := webrtc.SettingEngine{}

	udpLn, err := net.ListenUDP("udp", &net.UDPAddr{Port: cfg.Port})
	if err != nil {
		return nil, fmt.Errorf("RTC UDP :%d: %w", cfg.Port, err)
	}
	se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, udpLn))

	tcpLn, err := net.ListenTCP("tcp", &net.TCPAddr{Port: cfg.Port})
	if err != nil {
		log.Printf("RTC: TCP :%d недоступен (%v) — работаем только по UDP", cfg.Port, err)
		se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	} else {
		se.SetICETCPMux(webrtc.NewICETCPMux(nil, tcpLn, 64))
		se.SetNetworkTypes([]webrtc.NetworkType{
			webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6,
			webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6,
		})
	}
	// Не светим docker/WSL/vEthernet-интерфейсы только если это loopback; остальное оставляем —
	// среди них может быть Radmin/Hamachi/ZeroTier, через которые тоже должны работать.
	se.SetIncludeLoopbackCandidate(false)

	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}

	api := webrtc.NewAPI(webrtc.WithSettingEngine(se), webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir))
	return &SFU{cfg: cfg, api: api, rooms: make(map[string]*room)}, nil
}

type room struct {
	id     string
	mu     sync.Mutex
	peers  map[string]*Peer
	tracks map[string]*pubTrack // key: ownerID + "/" + kind
}

type pubTrack struct {
	key    string
	owner  *Peer
	remote *webrtc.TrackRemote
	local  *webrtc.TrackLocalStaticRTP
}

type Peer struct {
	ID     string
	sfu    *SFU
	room   *room
	pc     *webrtc.PeerConnection
	signal Signal

	serverMuted atomic.Bool // админ заглушил: звук этого участника никому не пересылается

	mu         sync.Mutex
	closed     bool
	offering   bool // ждём answer на наш offer
	needOffer  bool // во время ожидания что-то поменялось — нужен ещё один раунд
	senders    map[string]*webrtc.RTPSender
	wantVideo  map[string]bool // какие видеотреки участник сейчас смотрит (ключ "<id>/screen|camera")
	pendingICE []webrtc.ICECandidateInit
	onClosed   func()
}

// Join создаёт соединение участника peerID в комнате roomID и сразу отправляет ему offer.
func (s *SFU) Join(roomID, peerID string, signal Signal, onClosed func()) (*Peer, error) {
	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeAudio, webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeVideo} {
		if _, err := pc.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			_ = pc.Close()
			return nil, err
		}
	}

	p := &Peer{ID: peerID, sfu: s, pc: pc, signal: signal, senders: map[string]*webrtc.RTPSender{}, onClosed: onClosed}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		p.signal("rtc-candidate", map[string]any{"candidate": init})
		for _, extra := range s.extraCandidates(c, init) {
			p.signal("rtc-candidate", map[string]any{"candidate": extra})
		}
	})

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		switch st {
		case webrtc.PeerConnectionStateFailed:
			log.Printf("RTC %s: соединение не установлено (failed)", peerID)
			p.Close()
		case webrtc.PeerConnectionStateConnected:
			log.Printf("RTC %s: подключён к %s", peerID, roomID)
		}
	})

	pc.OnTrack(func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		p.publish(remote, trackLabel(pc, receiver, remote))
	})

	// Порядок блокировок: s.mu -> r.mu (как в Close), чтобы пустую комнату не удалили у нас из-под рук.
	s.mu.Lock()
	r := s.rooms[roomID]
	if r == nil {
		r = &room{id: roomID, peers: map[string]*Peer{}, tracks: map[string]*pubTrack{}}
		s.rooms[roomID] = r
	}
	p.room = r
	r.mu.Lock()
	r.peers[peerID] = p
	r.syncPeerLocked(p)
	r.mu.Unlock()
	s.mu.Unlock()

	p.negotiate()
	return p, nil
}

// extraCandidates дублирует локальный кандидат с дополнительными адресами (внешний IP и т.п.).
// Сервер слушает один мультиплексированный порт, так что входящие проверки ICE на любой из
// этих адресов придут в тот же сокет и будут сопоставлены по ufrag.
func (s *SFU) extraCandidates(c *webrtc.ICECandidate, base webrtc.ICECandidateInit) []webrtc.ICECandidateInit {
	if s.cfg.PublicIPs == nil || c.Typ != webrtc.ICECandidateTypeHost {
		return nil
	}
	ip := net.ParseIP(c.Address)
	if ip == nil {
		return nil
	}
	var out []webrtc.ICECandidateInit
	for i, pub := range s.cfg.PublicIPs() {
		pip := net.ParseIP(pub)
		if pip == nil || pip.Equal(ip) || (pip.To4() == nil) != (ip.To4() == nil) {
			continue
		}
		proto := strings.ToLower(c.Protocol.String())
		cand := fmt.Sprintf("candidate:%s%d %d %s %d %s %d typ host",
			c.Foundation, 9+i, c.Component, proto, c.Priority-uint32(i+1), pip.String(), c.Port)
		if c.Protocol == webrtc.ICEProtocolTCP {
			cand += " tcptype " + c.TCPType
		}
		out = append(out, webrtc.ICECandidateInit{
			Candidate:     cand,
			SDPMid:        base.SDPMid,
			SDPMLineIndex: base.SDPMLineIndex,
		})
	}
	return out
}

// trackLabel — что это за входящий поток, по m-line: "audio", "screen" или "camera".
func trackLabel(pc *webrtc.PeerConnection, receiver *webrtc.RTPReceiver, remote *webrtc.TrackRemote) string {
	for _, t := range pc.GetTransceivers() {
		if t.Receiver() == receiver {
			switch t.Mid() {
			case "0":
				return "audio"
			case "1":
				return "screen"
			case "2":
				return "camera"
			}
		}
	}
	if remote.Kind() == webrtc.RTPCodecTypeAudio {
		return "audio"
	}
	return "screen"
}

// publish — пришёл трек от участника: создаём локальную копию и раздаём остальным.
// id трека у получателей — "<id участника>-<label>", по нему клиент отличает экран от камеры.
func (p *Peer) publish(remote *webrtc.TrackRemote, label string) {
	kind := remote.Kind().String() // "audio" | "video"
	local, err := webrtc.NewTrackLocalStaticRTP(remote.Codec().RTPCodecCapability, p.ID+"-"+label, p.ID)
	if err != nil {
		log.Printf("RTC %s: track: %v", p.ID, err)
		return
	}
	t := &pubTrack{key: p.ID + "/" + label, owner: p, remote: remote, local: local}

	r := p.room
	r.mu.Lock()
	r.tracks[t.key] = t
	r.syncAllLocked()
	r.mu.Unlock()

	if kind == "video" {
		p.requestKeyframe(remote)
	}

	buf := make([]byte, 1500)
	for {
		n, _, err := remote.Read(buf)
		if err != nil {
			break
		}
		if kind == "audio" && p.serverMuted.Load() {
			continue
		}
		if _, err := local.Write(buf[:n]); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			break
		}
	}

	r.mu.Lock()
	if r.tracks[t.key] == t {
		delete(r.tracks, t.key)
		r.syncAllLocked()
	}
	r.mu.Unlock()
}

func (p *Peer) requestKeyframe(remote *webrtc.TrackRemote) {
	_ = p.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(remote.SSRC())}})
}

func (r *room) syncAllLocked() {
	for _, p := range r.peers {
		r.syncPeerLocked(p)
	}
}

// syncPeerLocked приводит набор отправляемых участнику треков к актуальному и при изменениях
// запускает пересогласование.
func (r *room) syncPeerLocked(p *Peer) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	changed := false
	for key, sender := range p.senders {
		if t, ok := r.tracks[key]; !ok || sender.Track() != t.local || !p.wantsLocked(t) {
			_ = p.pc.RemoveTrack(sender)
			delete(p.senders, key)
			changed = true
		}
	}
	for key, t := range r.tracks {
		if t.owner == p {
			continue
		}
		if _, ok := p.senders[key]; ok || !p.wantsLocked(t) {
			continue
		}
		sender, err := p.pc.AddTrack(t.local)
		if err != nil {
			log.Printf("RTC %s: AddTrack %s: %v", p.ID, key, err)
			continue
		}
		p.senders[key] = sender
		changed = true
		go forwardRTCP(sender, t)
		if t.remote.Kind() == webrtc.RTPCodecTypeVideo {
			// новому зрителю нужен ключевой кадр, иначе картинка появится только со следующим
			go func(t *pubTrack) {
				time.Sleep(500 * time.Millisecond)
				t.owner.requestKeyframe(t.remote)
			}(t)
		}
	}
	p.mu.Unlock()
	if changed {
		go p.negotiate()
	}
}

// forwardRTCP читает RTCP от получателя и пересылает запросы ключевого кадра владельцу трека.
func forwardRTCP(sender *webrtc.RTPSender, t *pubTrack) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		if t.remote.Kind() != webrtc.RTPCodecTypeVideo {
			continue
		}
		for _, pkt := range pkts {
			switch pkt.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				t.owner.requestKeyframe(t.remote)
			}
		}
	}
}

func (p *Peer) negotiate() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	if p.offering {
		p.needOffer = true
		p.mu.Unlock()
		return
	}
	p.offering = true
	p.needOffer = false
	p.mu.Unlock()

	offer, err := p.pc.CreateOffer(nil)
	var gathered <-chan struct{}
	if err == nil {
		gathered = webrtc.GatheringCompletePromise(p.pc)
		err = p.pc.SetLocalDescription(offer)
	}
	if err != nil {
		log.Printf("RTC %s: offer: %v", p.ID, err)
		p.mu.Lock()
		p.offering = false
		p.mu.Unlock()
		return
	}
	// Кандидаты у нас только локальные (один мультиплексированный порт), собираются за миллисекунды.
	// Отправляем offer уже с ними внутри SDP: через интернет отдельные trickle-кандидаты могли
	// прийти раньше, чем клиент применит offer, и потеряться.
	select {
	case <-gathered:
	case <-time.After(2 * time.Second):
	}
	sdp := offer.SDP
	if ld := p.pc.LocalDescription(); ld != nil {
		sdp = ld.SDP
	}
	p.signal("rtc-offer", map[string]any{"sdp": sdp})
}

// SetServerMuted включает/выключает серверное заглушение микрофона участника.
func (p *Peer) SetServerMuted(v bool) { p.serverMuted.Store(v) }

// wantsLocked — пересылать ли участнику этот трек. Звук — всегда; видео (экран, камеру) — только
// то, что он сейчас смотрит: иначе браузер принимал бы и декодировал чужие трансляции впустую,
// а это заметно грузит процессор и сеть. Вызывать под p.mu.
func (p *Peer) wantsLocked(t *pubTrack) bool {
	if t.remote.Kind() != webrtc.RTPCodecTypeVideo {
		return true
	}
	return p.wantVideo[t.key]
}

// SetWantedVideo задаёт, какие видеотреки участник смотрит (ключи "<id участника>/screen|camera").
func (p *Peer) SetWantedVideo(keys []string) {
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		if len(want) >= 32 {
			break
		}
		want[k] = true
	}
	r := p.room
	r.mu.Lock()
	p.mu.Lock()
	p.wantVideo = want
	p.mu.Unlock()
	r.syncPeerLocked(p)
	r.mu.Unlock()
}

// HandleAnswer — ответ клиента на наш offer.
func (p *Peer) HandleAnswer(sdp string) error {
	err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp})
	p.mu.Lock()
	p.offering = false
	again := p.needOffer
	pending := p.pendingICE
	p.pendingICE = nil
	p.mu.Unlock()
	for _, c := range pending {
		_ = p.pc.AddICECandidate(c)
	}
	if err != nil {
		return err
	}
	if again {
		p.negotiate()
	}
	return nil
}

func (p *Peer) HandleCandidate(c webrtc.ICECandidateInit) error {
	p.mu.Lock()
	if p.pc.RemoteDescription() == nil {
		p.pendingICE = append(p.pendingICE, c)
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	return p.pc.AddICECandidate(c)
}

// Close отключает участника от комнаты. Безопасно вызывать несколько раз.
func (p *Peer) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	onClosed := p.onClosed
	p.mu.Unlock()

	r := p.room
	r.mu.Lock()
	if r.peers[p.ID] == p {
		delete(r.peers, p.ID)
	}
	for key, t := range r.tracks {
		if t.owner == p {
			delete(r.tracks, key)
		}
	}
	r.syncAllLocked()
	empty := len(r.peers) == 0
	r.mu.Unlock()

	_ = p.pc.Close()

	if empty {
		p.sfu.mu.Lock()
		if cur := p.sfu.rooms[r.id]; cur == r {
			r.mu.Lock()
			if len(r.peers) == 0 {
				delete(p.sfu.rooms, r.id)
			}
			r.mu.Unlock()
		}
		p.sfu.mu.Unlock()
	}
	if onClosed != nil {
		onClosed()
	}
}
