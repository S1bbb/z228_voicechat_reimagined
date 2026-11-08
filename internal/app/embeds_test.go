package app

import (
	"net"
	"net/url"
	"testing"
)

func TestExtractLinks(t *testing.T) {
	got := extractLinks("глянь https://youtu.be/dQw4w9WgXcQ, и https://store.steampowered.com/app/730/ ещё `https://code.example/x` и https://a.ru/1 https://b.ru/2 https://c.ru/3")
	want := []string{"https://youtu.be/dQw4w9WgXcQ", "https://store.steampowered.com/app/730/", "https://a.ru/1"}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%v", got)
		}
	}
}

func TestYouTubeID(t *testing.T) {
	for raw, want := range map[string]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=10": "dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ":                     "dQw4w9WgXcQ",
		"https://youtube.com/shorts/dQw4w9WgXcQ":           "dQw4w9WgXcQ",
		"https://m.youtube.com/watch?v=dQw4w9WgXcQ":        "dQw4w9WgXcQ",
		"https://www.youtube.com/@channel":                 "",
		"https://notyoutube.com/watch?v=dQw4w9WgXcQ":       "",
	} {
		u, _ := url.Parse(raw)
		if got := youTubeID(u); got != want {
			t.Errorf("%s: %q", raw, got)
		}
	}
}

func TestPublicIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "82.47.130.62": true, "2a00:1450::1": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "172.16.0.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::1": false, "fd00::1": false, "fe80::1": false,
	} {
		if got := publicIP(net.ParseIP(ip)); got != want {
			t.Errorf("%s: %v", ip, got)
		}
	}
}

func TestParseMeta(t *testing.T) {
	base, _ := url.Parse("https://store.steampowered.com/app/730/")
	page := `<html><head><title>Fallback</title>
<meta property="og:site_name" content="Steam">
<meta property="og:title" content="Counter-Strike 2 &amp; Co">
<meta name="description" content="  Описание   игры ">
<meta property="og:image" content="/img/cs2.jpg">
<meta name="theme-color" content="#171a21">
</head><body><meta property="og:title" content="не отсюда"></body></html>`
	e := parseMeta(page, base)
	if e == nil || e.Site != "Steam" || e.Title != "Counter-Strike 2 & Co" || e.Description != "Описание игры" ||
		e.Image != "https://store.steampowered.com/img/cs2.jpg" || e.Color != "#171a21" {
		t.Fatalf("%+v", e)
	}
	if parseMeta("<html><head></head></html>", base) != nil {
		t.Fatal("пустая страница — без превью")
	}
}
