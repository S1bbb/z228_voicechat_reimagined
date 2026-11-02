package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	// HTTP
	Port       int
	HTTPSPort  int
	ListenHost string
	DataDir    string
	WebDir     string // если задан — статика берётся с диска (для разработки)

	// Сервер
	ServerName     string
	ServerSlogan   string // подпись под названием сервера
	ServerPassword string // пароль для входа на сервер (пусто — вход открыт)
	AdminPassword  string // пусто — сгенерируется и сохранится в БД
	MaxUploadMB    int

	// TLS
	TLSDomain    string // Let's Encrypt (autocert)
	TLSEmail     string
	TLSCertFile  string
	TLSKeyFile   string
	CertExtraIPs string // доп. IP для self-signed сертификата

	// WebRTC (SFU)
	RTCPort         int      // один порт UDP+TCP для всего медиа
	RTCPublicIPs    []string // адреса, которые сообщаем клиентам в дополнение к локальным
	RTCAutoPublicIP bool     // определить внешний IP через STUN при старте
	RTCStunServer   string
}

func Load() Config {
	return Config{
		Port:       envInt("PORT", 3000),
		HTTPSPort:  envInt("HTTPS_PORT", 3443),
		ListenHost: envString("LISTEN_HOST", ""),
		DataDir:    envString("DATA_DIR", "data"),
		WebDir:     envString("WEB_DIR", ""),

		ServerName:     envString("SERVER_NAME", "ZALUPA228"),
		ServerSlogan:   envString("SERVER_SLOGAN", "кто не с нами - сосите хуй"),
		ServerPassword: envString("SERVER_PASSWORD", ""),
		AdminPassword:  envString("ADMIN_PASSWORD", envString("ADMIN_PANEL_PASSWORD", "")),
		MaxUploadMB:    envInt("MAX_UPLOAD_MB", 25),

		TLSDomain:    envString("TLS_DOMAIN", ""),
		TLSEmail:     envString("TLS_EMAIL", ""),
		TLSCertFile:  envString("TLS_CERT_FILE", ""),
		TLSKeyFile:   envString("TLS_KEY_FILE", ""),
		CertExtraIPs: envString("CERT_EXTRA_IPS", ""),

		RTCPort:         envInt("RTC_PORT", 50000),
		RTCPublicIPs:    splitCSV(envString("RTC_PUBLIC_IP", "")),
		RTCAutoPublicIP: envBool("RTC_AUTO_PUBLIC_IP", true),
		RTCStunServer:   envString("RTC_STUN_SERVER", "stun.l.google.com:19302"),
	}
}

func envString(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envBool(k string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
