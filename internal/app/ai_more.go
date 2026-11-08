package app

import (
	"context"
	"encoding/base64"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"zalupa228local/internal/store"
)

// ---------- картинки: отдельная модель со зрением (у DeepSeek его нет) ----------

const (
	visionSettingURL   = "vision_url"
	visionSettingModel = "vision_model"
	visionSettingKey   = "vision_key"

	aiMaxImages    = 3
	aiMaxImageSize = 5 << 20
	aiImageRecent  = 10 * time.Minute // «что на скрине» сразу после чужой картинки
)

type visionCfg struct{ url, model, key string }

func (v visionCfg) ok() bool { return v.url != "" && v.model != "" && v.key != "" }

func (h *Hub) visionConfig(ctx context.Context) visionCfg {
	st := h.opts.Store
	u, _, _ := st.GetSetting(ctx, visionSettingURL)
	m, _, _ := st.GetSetting(ctx, visionSettingModel)
	k, _, _ := st.GetSetting(ctx, visionSettingKey)
	return visionCfg{url: u, model: m, key: k}
}

func imageAtts(m store.Message) []store.Attachment {
	var out []store.Attachment
	for _, a := range m.Attachments {
		switch a.Mime {
		case "image/png", "image/jpeg", "image/webp":
			out = append(out, a)
		}
	}
	return out
}

// aiImages — картинки, о которых спрашивают: из самого запроса, из сообщения, на которое ответили,
// или из свежего сообщения выше. Отдаются как data: URL (файлы лежат у нас, наружу ссылок нет).
func (h *Hub) aiImages(ctx context.Context, q store.Message, hist []store.Message) []string {
	atts := imageAtts(q)
	if len(atts) == 0 && q.ReplyTo != nil {
		if r, err := h.opts.Store.Message(ctx, q.ReplyTo.ID); err == nil && r.ChannelID == q.ChannelID {
			atts = imageAtts(r)
		}
	}
	if len(atts) == 0 {
		for i, n := len(hist)-1, 0; i >= 0 && n < 4; i, n = i-1, n+1 {
			m := hist[i]
			if q.CreatedAt-m.CreatedAt > aiImageRecent.Milliseconds() {
				break
			}
			if a := imageAtts(m); len(a) > 0 {
				atts = a
				break
			}
		}
	}
	var out []string
	for _, a := range atts {
		if len(out) == aiMaxImages {
			break
		}
		name := path.Base(a.URL)
		if !uploadURL.MatchString("/uploads/" + name) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(h.opts.UploadDir, name))
		if err != nil || len(data) > aiMaxImageSize {
			continue
		}
		out = append(out, "data:"+a.Mime+";base64,"+base64.StdEncoding.EncodeToString(data))
	}
	return out
}

// ---------- память: модель пишет в конце ответа «ЗАПОМНИТЬ: ник | факт» или «ЗАБЫТЬ: ник» ----------

var memOpRe = regexp.MustCompile(`(?mi)^[ \t>*_` + "`" + `-]*(ЗАПОМНИТЬ|ЗАБЫТЬ)[*_]*\s*:\s*(.+?)\s*$`)

type memOp struct {
	forget bool
	nick   string
	note   string
}

const aiMaxMemOps = 3

// extractMemoryOps вырезает служебные строки памяти из ответа.
func extractMemoryOps(answer string) (string, []memOp) {
	var ops []memOp
	clean := memOpRe.ReplaceAllStringFunc(answer, func(line string) string {
		sm := memOpRe.FindStringSubmatch(line)
		if len(ops) >= aiMaxMemOps {
			return ""
		}
		body := strings.Trim(sm[2], " *_`")
		if strings.EqualFold(sm[1], "ЗАБЫТЬ") {
			if n := cleanMemNick(body); n != "" {
				ops = append(ops, memOp{forget: true, nick: n})
			}
			return ""
		}
		nick, note, ok := strings.Cut(body, "|")
		if !ok {
			return ""
		}
		if n, t := cleanMemNick(nick), strings.TrimSpace(note); n != "" && t != "" {
			ops = append(ops, memOp{nick: n, note: t})
		}
		return ""
	})
	return strings.TrimSpace(clean), ops
}

func cleanMemNick(n string) string {
	n = strings.Trim(strings.TrimSpace(n), "@[]«»\"' ")
	if r := []rune(n); len(r) > 32 {
		return ""
	}
	return n
}

// applyMemoryOps сохраняет заметки; ник приводим к тому, как человек пишется на сервере.
func (h *Hub) applyMemoryOps(ctx context.Context, by string, ops []memOp, answer string) string {
	st := h.opts.Store
	saved := false
	for _, op := range ops {
		nick := op.nick
		if strings.EqualFold(nick, "я") || strings.EqualFold(nick, "меня") {
			nick = by
		}
		if v, ok, _ := st.MemberNick(ctx, nick); ok {
			nick = v
		}
		var err error
		if op.forget {
			err = st.AIForget(ctx, nick)
			log.Printf("ИИ: %s попросил забыть %s", by, nick)
		} else {
			err = st.AIRemember(ctx, nick, op.note, by)
			saved = true
			log.Printf("ИИ: %s попросил запомнить про %s", by, nick)
		}
		if err != nil {
			log.Printf("ИИ: память: %v", err)
		}
	}
	if answer == "" {
		if saved {
			return "Запомнил 🧠"
		}
		return "Забыл. Кто это вообще? 🫥"
	}
	return answer
}
