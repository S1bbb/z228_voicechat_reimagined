package app

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"zalupa228local/internal/store"
)

// Превью ссылок: после отправки сообщения сервер сходит по ссылкам, вытащит og:title/описание/картинку
// и разошлёт «message-update». Внутренние адреса (localhost, локалка, VPS сам себе) не трогаем.

const (
	embedMaxLinks = 3
	embedTimeout  = 6 * time.Second
	embedMaxBody  = 768 << 10
	embedCacheMax = 500
	embedCacheTTL = time.Hour
	embedUA       = "Mozilla/5.0 (compatible; z228-linkpreview/1.0; +https://github.com/S1bbb/z228_voicechat_reimagined)"
)

var linkRe = regexp.MustCompile(`\bhttps?://[^\s<]+[^\s<.,:;"')\]!?]`)

// extractLinks — ссылки из текста (как их подсвечивает клиент), без повторов и без тех, что в `коде`.
func extractLinks(text string) []string {
	text = regexp.MustCompile("(?s)```.*?```|`[^`\n]*`").ReplaceAllString(text, " ")
	var out []string
	seen := map[string]bool{}
	for _, u := range linkRe.FindAllString(text, -1) {
		if seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
		if len(out) == embedMaxLinks {
			break
		}
	}
	return out
}

var errPrivate = errors.New("внутренний адрес")

// publicIP — адрес из интернета (не loopback, не локалка, не link-local, не CGNAT и т.п.).
func publicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 0, v4[0] >= 240, v4[0] == 100 && v4[1]&0xc0 == 64, v4[0] == 192 && v4[1] == 0 && v4[2] == 0, v4[0] == 198 && v4[1]&0xfe == 18:
			return false
		}
	}
	return true
}

var embedClient = &http.Client{
	Timeout: embedTimeout,
	Transport: &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout: 4 * time.Second,
			// проверяем уже разрешённый IP — так не обойти через DNS и редиректы
			Control: func(_, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				if !publicIP(net.ParseIP(host)) {
					return errPrivate
				}
				return nil
			},
		}).DialContext,
		TLSHandshakeTimeout:   4 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		MaxIdleConns:          20,
		IdleConnTimeout:       30 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return errors.New("много редиректов")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return errors.New("не http")
		}
		return nil
	},
}

type embedCacheEntry struct {
	e  *store.Embed
	at time.Time
}

var embedCache = struct {
	sync.Mutex
	m map[string]embedCacheEntry
}{m: map[string]embedCacheEntry{}}

func cachedEmbed(ctx context.Context, raw string) *store.Embed {
	embedCache.Lock()
	if c, ok := embedCache.m[raw]; ok && time.Since(c.at) < embedCacheTTL {
		embedCache.Unlock()
		return c.e
	}
	embedCache.Unlock()
	e := fetchEmbed(ctx, raw)
	embedCache.Lock()
	if len(embedCache.m) >= embedCacheMax {
		for k, v := range embedCache.m {
			if time.Since(v.at) > embedCacheTTL || len(embedCache.m) >= embedCacheMax {
				delete(embedCache.m, k)
			}
		}
	}
	embedCache.m[raw] = embedCacheEntry{e, time.Now()}
	embedCache.Unlock()
	return e
}

var ytIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// youTubeID — id ролика из ссылки YouTube ("" — не ролик).
func youTubeID(u *url.URL) string {
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	host = strings.TrimPrefix(host, "m.")
	var id string
	switch host {
	case "youtu.be":
		id = strings.Trim(u.Path, "/")
	case "youtube.com", "music.youtube.com":
		switch {
		case u.Path == "/watch":
			id = u.Query().Get("v")
		case strings.HasPrefix(u.Path, "/shorts/"), strings.HasPrefix(u.Path, "/live/"), strings.HasPrefix(u.Path, "/embed/"):
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) > 1 {
				id = parts[1]
			}
		}
	}
	if ytIDRe.MatchString(id) {
		return id
	}
	return ""
}

var imageExt = regexp.MustCompile(`(?i)\.(png|jpe?g|gif|webp|avif)$`)

func fetchEmbed(ctx context.Context, raw string) *store.Embed {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil
	}
	if imageExt.MatchString(u.Path) {
		return &store.Embed{URL: raw, Kind: "image", Image: raw}
	}
	if id := youTubeID(u); id != "" {
		e := &store.Embed{URL: raw, Kind: "video", Site: "YouTube", VideoID: id, Image: "https://i.ytimg.com/vi/" + id + "/hqdefault.jpg", Color: "#ff0000"}
		// название и канал — из oEmbed (страница YouTube бывает «согласием на cookies»)
		var oe struct {
			Title  string `json:"title"`
			Author string `json:"author_name"`
		}
		if body, _, err := embedGet(ctx, "https://www.youtube.com/oembed?format=json&url="+url.QueryEscape("https://www.youtube.com/watch?v="+id), 64<<10); err == nil {
			if json.Unmarshal(body, &oe) == nil {
				e.Title = clipText(oe.Title, 200)
				if oe.Author != "" {
					e.Site = "YouTube · " + clipText(oe.Author, 80)
				}
			}
		}
		return e
	}
	body, ctype, err := embedGet(ctx, raw, embedMaxBody)
	if err != nil {
		return nil
	}
	if strings.HasPrefix(ctype, "image/") {
		return &store.Embed{URL: raw, Kind: "image", Image: raw}
	}
	if ctype != "text/html" && ctype != "application/xhtml+xml" {
		return nil
	}
	e := parseMeta(string(body), u)
	if e == nil {
		return nil
	}
	e.URL = raw
	return e
}

func embedGet(ctx context.Context, raw string, limit int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", embedUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,image/*;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "ru,en;q=0.8")
	resp, err := embedClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", errors.New(resp.Status)
	}
	ctype, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(ctype, "image/") {
		return nil, ctype, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	return b, ctype, err
}

var (
	metaTagRe  = regexp.MustCompile(`(?is)<meta\s[^>]*>`)
	attrRe     = regexp.MustCompile(`(?is)([a-z:-]+)\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	titleTagRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	colorRe    = regexp.MustCompile(`^#[0-9a-fA-F]{3,8}$`)
)

// parseMeta вытаскивает og:/twitter:-теги из начала страницы.
func parseMeta(page string, base *url.URL) *store.Embed {
	if i := strings.Index(strings.ToLower(page), "</head>"); i > 0 {
		page = page[:i]
	}
	meta := map[string]string{}
	for _, tag := range metaTagRe.FindAllString(page, 200) {
		attrs := map[string]string{}
		for _, a := range attrRe.FindAllStringSubmatch(tag, -1) {
			attrs[strings.ToLower(a[1])] = html.UnescapeString(strings.Trim(a[2], `"'`))
		}
		key := strings.ToLower(attrs["property"])
		if key == "" {
			key = strings.ToLower(attrs["name"])
		}
		if key != "" && attrs["content"] != "" {
			if _, dup := meta[key]; !dup {
				meta[key] = strings.TrimSpace(attrs["content"])
			}
		}
	}
	first := func(keys ...string) string {
		for _, k := range keys {
			if v := meta[k]; v != "" {
				return v
			}
		}
		return ""
	}
	e := &store.Embed{
		Site:        clipText(first("og:site_name", "application-name"), 80),
		Title:       clipText(first("og:title", "twitter:title"), 200),
		Description: clipText(first("og:description", "twitter:description", "description"), 300),
	}
	if e.Title == "" {
		if m := titleTagRe.FindStringSubmatch(page); m != nil {
			e.Title = clipText(strings.Join(strings.Fields(html.UnescapeString(m[1])), " "), 200)
		}
	}
	if img := first("og:image:secure_url", "og:image", "twitter:image", "twitter:image:src"); img != "" {
		if iu, err := base.Parse(img); err == nil && (iu.Scheme == "https" || iu.Scheme == "http") {
			e.Image = iu.String()
		}
	}
	if c := first("theme-color"); colorRe.MatchString(c) {
		e.Color = c
	}
	if e.Site == "" {
		e.Site = strings.TrimPrefix(base.Hostname(), "www.")
	}
	if e.Title == "" && e.Description == "" && e.Image == "" {
		return nil
	}
	return e
}

func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// embedSem — сколько сообщений одновременно разбираем
var embedSem = make(chan struct{}, 4)

// fetchEmbeds собирает превью для сообщения и рассылает обновление.
func (h *Hub) fetchEmbeds(msg store.Message, dmMembers []string) {
	links := extractLinks(msg.Text)
	if len(links) == 0 {
		return
	}
	go func() {
		embedSem <- struct{}{}
		defer func() { <-embedSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var embeds []store.Embed
		for _, l := range links {
			if e := cachedEmbed(ctx, l); e != nil {
				embeds = append(embeds, *e)
			}
		}
		if len(embeds) == 0 {
			return
		}
		out, ok, err := h.opts.Store.SetEmbeds(context.Background(), msg.ID, msg.Text, embeds)
		if err != nil {
			log.Printf("превью: %v", err)
			return
		}
		if ok {
			h.broadcastIn(dmMembers, "message-update", map[string]any{"message": out}, nil)
		}
	}()
}
