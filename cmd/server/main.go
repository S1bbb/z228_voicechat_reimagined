package main

import (
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"zalupa228local/internal/app"
	"zalupa228local/internal/cert"
	"zalupa228local/internal/config"
	"zalupa228local/internal/netinfo"
	"zalupa228local/internal/sfu"
	"zalupa228local/internal/store"
	"zalupa228local/web"
)

var version = "dev"

func main() {
	cfg := config.Load()

	// Флаги перекрывают переменные окружения.
	fl := flag.NewFlagSet("z228", flag.ExitOnError)
	fl.Usage = func() {
		fmt.Fprintf(os.Stderr, `z228 — самохостный голосовой чат в стиле Discord

Запуск:
  z228                   обычный запуск (локалка + интернет, если проброшены порты)
  z228 -radmin           для игры по Radmin VPN: ссылка на Radmin-IP, без определения внешнего IP
  z228 -lan              только локальная сеть / VPN, без определения внешнего IP
  z228 -open             сразу открыть чат в браузере

Порты, которые нужно открыть друзьям (в роутере или файрволе):
  TCP  %d   — сайт (HTTPS)
  UDP  %d   — голос и экран (и TCP %d как запасной)

Флаги (у каждого есть переменная окружения, см. README):
`, cfg.HTTPSPort, cfg.RTCPort, cfg.RTCPort)
		fl.PrintDefaults()
	}
	radmin := fl.Bool("radmin", false, "режим Radmin VPN: показать ссылку по Radmin-IP, не определять внешний IP")
	lanOnly := fl.Bool("lan", false, "только LAN/VPN: не определять внешний IP")
	open := fl.Bool("open", false, "открыть чат в браузере после запуска")
	showVersion := fl.Bool("version", false, "показать версию")
	publicIP := fl.String("public-ip", strings.Join(cfg.RTCPublicIPs, ","), "доп. адреса для голоса через запятую (внешний IP, IP VPN)")
	fl.IntVar(&cfg.Port, "port", cfg.Port, "HTTP-порт")
	fl.IntVar(&cfg.HTTPSPort, "https-port", cfg.HTTPSPort, "HTTPS-порт (0 — выключить, например за reverse proxy)")
	fl.IntVar(&cfg.RTCPort, "rtc-port", cfg.RTCPort, "порт для голоса (UDP + TCP)")
	fl.StringVar(&cfg.DataDir, "data", cfg.DataDir, "каталог данных (БД, загрузки, сертификаты)")
	fl.StringVar(&cfg.ServerName, "name", cfg.ServerName, "название сервера")
	fl.StringVar(&cfg.ServerSlogan, "slogan", cfg.ServerSlogan, "подпись под названием сервера")
	fl.StringVar(&cfg.ServerPassword, "password", cfg.ServerPassword, "пароль для входа на сервер (пусто — без пароля)")
	fl.StringVar(&cfg.AdminPassword, "admin-password", cfg.AdminPassword, "пароль администратора (пусто — сгенерировать)")
	fl.StringVar(&cfg.TLSDomain, "domain", cfg.TLSDomain, "домен для бесплатного сертификата Let's Encrypt")
	fl.StringVar(&cfg.WebDir, "web-dir", cfg.WebDir, "брать фронтенд с диска (для разработки)")
	_ = fl.Parse(os.Args[1:])

	if *showVersion {
		fmt.Println("z228", version)
		return
	}
	cfg.RTCPublicIPs = splitCSV(*publicIP)
	if *radmin || *lanOnly {
		cfg.RTCAutoPublicIP = false
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("БД: %v", err)
	}
	defer st.Close()

	adminPassword, generated := resolveAdminPassword(st, cfg.AdminPassword)

	ips := &ipSet{static: cfg.RTCPublicIPs}
	if cfg.RTCAutoPublicIP {
		go ips.autodetect(cfg.RTCStunServer)
	}

	media, err := sfu.New(sfu.Config{Port: cfg.RTCPort, PublicIPs: ips.list})
	if err != nil {
		log.Fatalf("SFU: %v\n(порт %d занят? укажите другой: -rtc-port)", err, cfg.RTCPort)
	}

	uploadDir := filepath.Join(cfg.DataDir, "uploads")
	hub := app.NewHub(app.Options{
		Store:          st,
		SFU:            media,
		ServerName:     cfg.ServerName,
		ServerSlogan:   cfg.ServerSlogan,
		ServerPassword: cfg.ServerPassword,
		AdminPassword:  adminPassword,
		UploadDir:      uploadDir,
		MaxUploadBytes: int64(cfg.MaxUploadMB) << 20,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.ServeWS)
	mux.HandleFunc("/api/upload", hub.HandleUpload)
	mux.Handle("/uploads/", hub.UploadsHandler())
	mux.Handle("/", staticHandler(cfg.WebDir))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var servers []*http.Server
	var tlsConf *tls.Config
	var httpHandler http.Handler = mux

	addrs := netinfo.LocalIPv4()
	switch {
	case cfg.TLSDomain != "":
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.TLSDomain),
			Cache:      autocert.DirCache(filepath.Join(cfg.DataDir, "certs")),
			Email:      cfg.TLSEmail,
		}
		tlsConf = m.TLSConfig()
		httpHandler = m.HTTPHandler(redirectToHTTPS(cfg.HTTPSPort, mux))
	case cfg.TLSCertFile != "" && cfg.TLSKeyFile != "":
		c, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			log.Fatalf("TLS: %v", err)
		}
		tlsConf = &tls.Config{Certificates: []tls.Certificate{c}}
		httpHandler = redirectToHTTPS(cfg.HTTPSPort, mux)
	case cfg.HTTPSPort > 0:
		var sans []net.IP
		for _, a := range addrs {
			sans = append(sans, a.IP)
		}
		for _, s := range append(splitCSV(cfg.CertExtraIPs), cfg.RTCPublicIPs...) {
			if ip := net.ParseIP(s); ip != nil {
				sans = append(sans, ip)
			}
		}
		c, err := cert.LoadOrCreate(cfg.DataDir, sans)
		if err != nil {
			log.Fatalf("сертификат: %v", err)
		}
		tlsConf = &tls.Config{Certificates: []tls.Certificate{c}}
		httpHandler = redirectToHTTPS(cfg.HTTPSPort, mux)
	}

	httpSrv := &http.Server{Addr: net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.Port)), Handler: httpHandler, ReadHeaderTimeout: 10 * time.Second}
	servers = append(servers, httpSrv)
	go serve(httpSrv, nil)

	if tlsConf != nil && cfg.HTTPSPort > 0 {
		httpsSrv := &http.Server{Addr: net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.HTTPSPort)), Handler: mux, TLSConfig: tlsConf, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, httpsSrv)
		go serve(httpsSrv, tlsConf)
	}

	time.Sleep(150 * time.Millisecond)
	printBanner(cfg, addrs, ips, adminPassword, generated, *radmin)
	if *open {
		// На своём ПК — через localhost: там микрофон работает без HTTPS и без предупреждений о сертификате.
		openBrowser(fmt.Sprintf("http://localhost:%d", cfg.Port))
	}

	<-ctx.Done()
	log.Println("Остановка...")
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shCtx)
	}
}

func serve(s *http.Server, tlsConf *tls.Config) {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		log.Fatalf("не удалось занять %s: %v", s.Addr, err)
	}
	if tlsConf != nil {
		ln = tls.NewListener(ln, tlsConf)
	}
	if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("HTTP %s: %v", s.Addr, err)
	}
}

// redirectToHTTPS отправляет на HTTPS всех, кроме localhost (там микрофон работает и по HTTP)
// и запросов через reverse proxy.
func redirectToHTTPS(httpsPort int, next http.Handler) http.Handler {
	if httpsPort <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		if host == "localhost" || host == "127.0.0.1" || host == "::1" || r.Header.Get("X-Forwarded-Proto") != "" {
			next.ServeHTTP(w, r)
			return
		}
		target := "https://" + net.JoinHostPort(host, strconv.Itoa(httpsPort)) + r.URL.RequestURI()
		if httpsPort == 443 {
			target = "https://" + host + r.URL.RequestURI()
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	})
}

func staticHandler(dir string) http.Handler {
	var fsys fs.FS
	if dir != "" {
		fsys = os.DirFS(dir)
	} else {
		sub, err := fs.Sub(web.Static, "static")
		if err != nil {
			log.Fatal(err)
		}
		fsys = sub
	}
	files := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Фронтенд маленький — пусть браузер всегда проверяет свежесть, чтобы обновления доезжали сразу.
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		files.ServeHTTP(w, r)
	})
}

func resolveAdminPassword(st *store.Store, fromCfg string) (string, bool) {
	if fromCfg != "" {
		return fromCfg, false
	}
	ctx := context.Background()
	if v, ok, _ := st.GetSetting(ctx, "admin_password"); ok && v != "" {
		return v, false
	}
	b := make([]byte, 6)
	_, _ = crand.Read(b)
	pw := fmt.Sprintf("%x", b)
	if err := st.SetSetting(ctx, "admin_password", pw); err != nil {
		log.Printf("не удалось сохранить пароль админа: %v", err)
	}
	return pw, true
}

// ---------- внешние адреса для голоса ----------

type ipSet struct {
	mu       sync.RWMutex
	static   []string
	detected string
}

func (s *ipSet) list() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]string(nil), s.static...)
	if s.detected != "" {
		out = append(out, s.detected)
	}
	return out
}

func (s *ipSet) get() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.detected
}

func (s *ipSet) autodetect(stunServer string) {
	for {
		ip, err := netinfo.PublicIP(stunServer, 3*time.Second)
		if err == nil && ip != nil {
			s.mu.Lock()
			if s.detected != ip.String() {
				if s.detected != "" {
					log.Printf("Внешний IP изменился: %s", ip)
				}
				s.detected = ip.String()
			}
			s.mu.Unlock()
		}
		time.Sleep(10 * time.Minute)
	}
}

// ---------- баннер ----------

func printBanner(cfg config.Config, addrs []netinfo.Addr, ips *ipSet, adminPw string, generated, radminMode bool) []string {
	scheme, port := "https", cfg.HTTPSPort
	if cfg.HTTPSPort <= 0 {
		scheme, port = "http", cfg.Port
	}
	url := func(host string) string {
		if (scheme == "https" && port == 443) || (scheme == "http" && port == 80) {
			return scheme + "://" + host
		}
		return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port))
	}

	var links []string
	fmt.Println()
	fmt.Println("  ╔══════════════════════════════════════════════╗")
	fmt.Printf("  ║  z228 %-38s ║\n", version+" — "+cfg.ServerName)
	fmt.Println("  ╚══════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Printf("  Этот ПК:      http://localhost:%d\n", cfg.Port)

	if cfg.TLSDomain != "" {
		fmt.Printf("  Домен:        %s\n", url(cfg.TLSDomain))
		links = append(links, url(cfg.TLSDomain))
	}

	radmins := netinfo.ByKind(addrs, "radmin")
	for _, kind := range []string{"radmin", "hamachi", "zerotier", "tailscale", "lan"} {
		label := map[string]string{"radmin": "Radmin VPN:", "hamachi": "Hamachi:", "zerotier": "ZeroTier:", "tailscale": "Tailscale:", "lan": "Локалка:"}[kind]
		for _, a := range netinfo.ByKind(addrs, kind) {
			fmt.Printf("  %-13s %s\n", label, url(a.IP.String()))
			links = append(links, url(a.IP.String()))
		}
	}
	if radminMode && len(radmins) == 0 {
		fmt.Println("  ⚠ Radmin VPN-адаптер не найден. Radmin запущен и вы в сети?")
	}

	if cfg.RTCAutoPublicIP {
		deadline := time.Now().Add(3 * time.Second)
		for ips.get() == "" && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if pub := ips.get(); pub != "" {
			fmt.Printf("  Интернет:     %s   (если проброшены порты)\n", url(pub))
		}
	}

	fmt.Println()
	if scheme == "https" && cfg.TLSDomain == "" && cfg.TLSCertFile == "" {
		fmt.Println("  Сертификат самоподписанный: при первом входе браузер предупредит —")
		fmt.Println("  нажмите «Дополнительно» → «Перейти на сайт».")
	}
	if cfg.RTCAutoPublicIP || len(cfg.RTCPublicIPs) > 0 {
		fmt.Printf("  Для друзей из интернета откройте: TCP %d, UDP %d (+ TCP %d).\n", port, cfg.RTCPort, cfg.RTCPort)
	}
	if cfg.ServerPassword != "" {
		fmt.Println("  Вход по паролю сервера включён.")
	}
	if generated {
		fmt.Printf("  Пароль админа (сгенерирован, сохранён в БД): %s\n", adminPw)
	} else {
		fmt.Println("  Пароль админа задан.")
	}
	fmt.Println()
	fmt.Println("  Ctrl+C — остановить.")
	fmt.Println()

	if radminMode && len(radmins) > 0 {
		links = append([]string{url(radmins[0].IP.String())}, links...)
	}
	if len(links) == 0 {
		links = append(links, fmt.Sprintf("http://localhost:%d", cfg.Port))
	}
	return links
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
