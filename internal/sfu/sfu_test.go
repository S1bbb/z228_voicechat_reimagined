package sfu

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// testClient — pion-клиент вместо браузера: принимает offer сервера, отвечает answer.
type testClient struct {
	t      *testing.T
	pc     *webrtc.PeerConnection
	mu     sync.Mutex
	offers []string
	peer   *Peer
	mute   bool // не отвечать на offer (как зависший/упавший браузер)
	drop   int  // столько следующих answer «теряется» по дороге к серверу
}

func freePort(t *testing.T) int {
	l, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.LocalAddr().(*net.UDPAddr).Port
}

func newTestSFU(t *testing.T) *SFU {
	// клиент и сервер на одной машине: сообщаем 127.0.0.1 как дополнительный адрес сервера
	s, err := New(Config{Port: freePort(t), PublicIPs: func() []string { return []string{"127.0.0.1"} }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newTestClient(t *testing.T, s *SFU, room, id string) *testClient {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	c := &testClient{t: t, pc: pc}
	answered := make(chan struct{}, 16)
	signal := func(typ string, payload map[string]any) {
		switch typ {
		case "rtc-offer":
			sdp := payload["sdp"].(string)
			c.mu.Lock()
			c.offers = append(c.offers, sdp)
			mute := c.mute
			c.mu.Unlock()
			if mute {
				return
			}
			go func() {
				if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
					t.Errorf("client SetRemoteDescription: %v", err)
					return
				}
				ans, err := pc.CreateAnswer(nil)
				if err == nil {
					err = pc.SetLocalDescription(ans)
				}
				if err != nil {
					t.Errorf("client answer: %v", err)
					return
				}
				<-webrtc.GatheringCompletePromise(pc)
				for c.peer == nil {
					time.Sleep(5 * time.Millisecond)
				}
				c.mu.Lock()
				drop := c.drop > 0
				if drop {
					c.drop--
				}
				c.mu.Unlock()
				if drop {
					return
				}
				if err := c.peer.HandleAnswer(pc.LocalDescription().SDP); err != nil {
					t.Errorf("HandleAnswer: %v", err)
				}
				answered <- struct{}{}
			}()
		case "rtc-candidate":
			init := payload["candidate"].(webrtc.ICECandidateInit)
			go func() {
				for pc.RemoteDescription() == nil {
					time.Sleep(5 * time.Millisecond)
				}
				_ = pc.AddICECandidate(init)
			}()
		}
	}
	p, err := s.Join(room, id, signal, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.peer = p
	c.mu.Unlock()
	return c
}

func (c *testClient) offerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.offers)
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("не дождались: %s", what)
}

func iceUfrag(sdp string) string {
	for _, l := range strings.Split(sdp, "\n") {
		if strings.HasPrefix(l, "a=ice-ufrag:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "a=ice-ufrag:"))
		}
	}
	return ""
}

func withAnswerTimeout(t *testing.T, d time.Duration) {
	old := answerTimeout
	answerTimeout = d
	t.Cleanup(func() { answerTimeout = old })
}

// Клиент не ответил на offer — сервер повторяет его, а после maxOfferRetries повторов
// закрывает соединение с пометкой Lost (клиент перезайдёт сам).
func TestOfferRetriedThenLost(t *testing.T) {
	withAnswerTimeout(t, 150*time.Millisecond)
	s := newTestSFU(t)
	c := &testClient{}
	closed := make(chan struct{})
	var p *Peer
	p, err := s.Join("1", "a", func(typ string, payload map[string]any) {
		if typ == "rtc-offer" {
			c.mu.Lock()
			c.offers = append(c.offers, payload["sdp"].(string))
			c.mu.Unlock()
		}
	}, func() { close(closed) })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("соединение без ответов не закрылось")
	}
	if got := c.offerCount(); got != maxOfferRetries+1 {
		t.Fatalf("offer отправлен %d раз, ждали %d", got, maxOfferRetries+1)
	}
	if !p.Lost() {
		t.Fatal("закрыто без пометки Lost — клиент не перезайдёт")
	}
}

// Один ответ потерялся — следующий offer согласуется, и звонок соединяется.
func TestLostAnswerRecovers(t *testing.T) {
	withAnswerTimeout(t, 300*time.Millisecond)
	s := newTestSFU(t)
	c := newTestClient(t, s, "1", "a")
	_ = c
	// первый offer уже ушёл и обработан; эмулируем потерю ответа на следующий
	waitFor(t, "соединение", 10*time.Second, func() bool { return c.pc.ConnectionState() == webrtc.PeerConnectionStateConnected })
	c.mu.Lock()
	c.mute = true
	c.mu.Unlock()
	n := c.offerCount()
	c.peer.negotiate()
	waitFor(t, "отправка offer", 2*time.Second, func() bool { return c.offerCount() == n+1 })
	c.mu.Lock()
	c.mute = false
	c.mu.Unlock()
	waitFor(t, "повторный offer после таймаута", 3*time.Second, func() bool { return c.offerCount() >= n+2 })
	waitFor(t, "согласование", 3*time.Second, func() bool {
		c.peer.mu.Lock()
		defer c.peer.mu.Unlock()
		return !c.peer.offering && c.peer.retries == 0
	})
	if c.peer.Lost() {
		t.Fatal("соединение помечено потерянным")
	}
}

// RestartICE шлёт offer с новыми ICE-учётками, и соединение остаётся живым.
func TestRestartICE(t *testing.T) {
	s := newTestSFU(t)
	c := newTestClient(t, s, "1", "a")
	waitFor(t, "соединение", 10*time.Second, func() bool { return c.pc.ConnectionState() == webrtc.PeerConnectionStateConnected })
	n := c.offerCount()
	c.mu.Lock()
	before := iceUfrag(c.offers[n-1])
	c.mu.Unlock()
	c.peer.RestartICE()
	waitFor(t, "offer с перезапуском ICE", 3*time.Second, func() bool { return c.offerCount() > n })
	c.mu.Lock()
	after := iceUfrag(c.offers[n])
	c.mu.Unlock()
	if before == "" || before == after {
		t.Fatalf("ufrag не сменился: %q -> %q", before, after)
	}
	waitFor(t, "соединение после перезапуска ICE", 10*time.Second, func() bool {
		c.peer.mu.Lock()
		done := !c.peer.offering
		c.peer.mu.Unlock()
		return done && c.pc.ConnectionState() == webrtc.PeerConnectionStateConnected && c.pc.ICEConnectionState() == webrtc.ICEConnectionStateConnected
	})
}

// Запоздавший ответ (offer уже согласован) не ломает состояние.
func TestStaleAnswerIgnored(t *testing.T) {
	s := newTestSFU(t)
	c := newTestClient(t, s, "1", "a")
	waitFor(t, "соединение", 10*time.Second, func() bool { return c.pc.ConnectionState() == webrtc.PeerConnectionStateConnected })
	if err := c.peer.HandleAnswer(c.pc.LocalDescription().SDP); err != nil {
		t.Fatalf("повторный ответ: %v", err)
	}
	c.peer.mu.Lock()
	defer c.peer.mu.Unlock()
	if c.peer.offering {
		t.Fatal("после лишнего ответа сервер считает, что ждёт answer")
	}
}

// Клиент ответил, но answer потерялся: сервер шлёт тот же offer ещё раз, клиент (уже в stable)
// применяет его повторно и отвечает — согласование завершается.
func TestDroppedAnswerRecovers(t *testing.T) {
	withAnswerTimeout(t, 300*time.Millisecond)
	s := newTestSFU(t)
	c := newTestClient(t, s, "1", "a")
	waitFor(t, "соединение", 10*time.Second, func() bool { return c.pc.ConnectionState() == webrtc.PeerConnectionStateConnected })
	c.mu.Lock()
	c.drop = 1
	c.mu.Unlock()
	n := c.offerCount()
	c.peer.negotiate()
	waitFor(t, "повтор offer и согласование", 4*time.Second, func() bool {
		c.peer.mu.Lock()
		defer c.peer.mu.Unlock()
		return c.offerCount() >= n+2 && !c.peer.offering && c.peer.retries == 0
	})
	if c.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
		t.Fatal("соединение пропало")
	}
}
