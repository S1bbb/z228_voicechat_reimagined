// Smoke-тест сервера: два клиента входят, переписываются и подключаются к голосу;
// проверяем, что звук A доходит до B через SFU.
//
//	go run ./tools/smoketest -url ws://127.0.0.1:3000/ws
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type client struct {
	name    string
	ws      *websocket.Conn
	wmu     sync.Mutex
	id      string
	pc      *webrtc.PeerConnection
	track   *webrtc.TrackLocalStaticSample
	msgs    chan map[string]any
	gotFrom chan string // stream id полученного аудиотрека
	packets chan int
	count   atomic.Int64
}

func dial(url, nick string) *client {
	// сертификат по умолчанию самоподписанный — для теста проверку не делаем
	d := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, HandshakeTimeout: 10 * time.Second}
	ws, _, err := d.Dial(url, nil)
	if err != nil {
		log.Fatalf("%s: dial: %v", nick, err)
	}
	c := &client{name: nick, ws: ws, msgs: make(chan map[string]any, 256), gotFrom: make(chan string, 4), packets: make(chan int, 4)}
	go func() {
		for {
			_, raw, err := ws.ReadMessage()
			if err != nil {
				close(c.msgs)
				return
			}
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			switch m["type"] {
			case "rtc-offer":
				go c.onOffer(m["sdp"].(string))
			case "rtc-candidate":
				b, _ := json.Marshal(m["candidate"])
				var ci webrtc.ICECandidateInit
				_ = json.Unmarshal(b, &ci)
				if c.pc != nil {
					_ = c.pc.AddICECandidate(ci)
				}
			default:
				c.msgs <- m
			}
		}
	}()
	c.send(map[string]any{"type": "hello", "nickname": nick, "clientId": "smoke-" + nick + "-0000"})
	m := c.wait("ready")
	c.id = m["sessionId"].(string)
	return c
}

func (c *client) send(m map[string]any) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.WriteJSON(m)
}

func (c *client) wait(typ string) map[string]any {
	timeout := time.After(10 * time.Second)
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				log.Fatalf("%s: соединение закрыто, ждали %s", c.name, typ)
			}
			if m["type"] == typ {
				return m
			}
			if m["type"] == "error" || m["type"] == "hello-rejected" {
				log.Fatalf("%s: %v", c.name, m)
			}
		case <-timeout:
			log.Fatalf("%s: таймаут ожидания %s", c.name, typ)
		}
	}
}

func (c *client) joinVoice(channelID float64) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Fatal(err)
	}
	c.pc = pc
	c.track, _ = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "mic", c.id)
	if _, err := pc.AddTrack(c.track); err != nil {
		log.Fatal(err)
	}
	pc.OnICECandidate(func(ci *webrtc.ICECandidate) {
		if ci != nil {
			c.send(map[string]any{"type": "rtc-candidate", "candidate": ci.ToJSON()})
		}
	})
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		c.gotFrom <- t.StreamID()
		n := 0
		buf := make([]byte, 1500)
		for {
			if _, _, err := t.Read(buf); err != nil {
				return
			}
			n++
			c.count.Add(1)
			if n == 20 {
				c.packets <- n
			}
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) { log.Printf("%s: RTC %s", c.name, s) })
	c.send(map[string]any{"type": "voice-join", "channelId": channelID})
}

func (c *client) onOffer(sdp string) {
	pc := c.pc
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		log.Fatalf("%s: setRemote: %v", c.name, err)
	}
	ans, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Fatalf("%s: answer: %v", c.name, err)
	}
	_ = pc.SetLocalDescription(ans)
	c.send(map[string]any{"type": "rtc-answer", "sdp": ans.SDP})
}

func (c *client) streamSilence(stop chan struct{}) {
	frame := []byte{0xf8, 0xff, 0xfe} // opus DTX-тишина
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			_ = c.track.WriteSample(media.Sample{Data: frame, Duration: 20 * time.Millisecond})
		}
	}
}

func main() {
	url := flag.String("url", "ws://127.0.0.1:3000/ws", "адрес WebSocket")
	adminPw := flag.String("admin", "", "пароль админа (проверить серверный мьют)")
	flag.Parse()

	a := dial(*url, "smokeA")
	b := dial(*url, "smokeB")
	fmt.Println("✓ оба клиента вошли")

	a.send(map[string]any{"type": "history", "channelId": 1})
	h := a.wait("history")
	fmt.Printf("✓ история канала 1: %d сообщений\n", len(h["messages"].([]any)))

	a.send(map[string]any{"type": "send", "channelId": 1, "text": "привет из smoke-теста **жирный**", "nonce": "n1"})
	m := b.wait("message")
	msg := m["message"].(map[string]any)
	fmt.Printf("✓ B получил сообщение #%v: %q\n", msg["id"], msg["text"])
	id := msg["id"]
	b.send(map[string]any{"type": "react", "id": id, "emoji": "🔥"})
	a.wait("message-update")
	fmt.Println("✓ реакция дошла")
	a.send(map[string]any{"type": "delete", "id": id})
	b.wait("message-delete")
	fmt.Println("✓ удаление дошло")

	// на чистой БД сид создаёт каналы 1–2 (текст) и 3–4 (голос)
	voiceID := float64(3)

	a.joinVoice(voiceID)
	a.wait("voice-joined")
	b.joinVoice(voiceID)
	b.wait("voice-joined")
	stop := make(chan struct{})
	go a.streamSilence(stop)

	select {
	case from := <-b.gotFrom:
		if from != a.id {
			log.Fatalf("B получил трек от %q, ожидали %q", from, a.id)
		}
		fmt.Println("✓ B получил аудиотрек A через SFU")
	case <-time.After(15 * time.Second):
		log.Fatal("✗ B не получил трек A за 15 с")
	}
	select {
	case <-b.packets:
		fmt.Println("✓ RTP-пакеты A доходят до B")
	case <-time.After(10 * time.Second):
		log.Fatal("✗ пакеты не идут")
	}
	if *adminPw != "" {
		b.send(map[string]any{"type": "admin-login", "password": *adminPw})
		if ok, _ := b.wait("admin")["ok"].(bool); !ok {
			log.Fatal("✗ admin-login не прошёл")
		}
		b.send(map[string]any{"type": "server-mute", "sessionId": a.id, "value": true})
		time.Sleep(300 * time.Millisecond)
		before := b.count.Load()
		time.Sleep(time.Second)
		if d := b.count.Load() - before; d > 0 {
			log.Fatalf("✗ после серверного мьюта пришло %d пакетов", d)
		}
		fmt.Println("✓ серверный мьют: звук A больше не пересылается")
		b.send(map[string]any{"type": "server-mute", "sessionId": a.id, "value": false})
		time.Sleep(300 * time.Millisecond)
		before = b.count.Load()
		time.Sleep(time.Second)
		if b.count.Load()-before < 10 {
			log.Fatal("✗ после снятия мьюта звук не вернулся")
		}
		fmt.Println("✓ мьют снят, звук снова идёт")
	}
	close(stop)
	a.send(map[string]any{"type": "voice-leave"})
	fmt.Println("OK")
	os.Exit(0)
}
