package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"zalupa228local/internal/store"
)

// Типы, которые безопасно показывать в браузере прямо со своего домена.
// Всё остальное (html, svg, js, ...) отдаётся как скачиваемый файл.
var inlineMime = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".avif": "image/avif",
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mov":  "video/quicktime",
	".mp3":  "audio/mpeg",
	".ogg":  "audio/ogg",
	".opus": "audio/ogg",
	".wav":  "audio/wav",
	".m4a":  "audio/mp4",
	".txt":  "text/plain; charset=utf-8",
}

func mimeForExt(ext string) string {
	if m, ok := inlineMime[strings.ToLower(ext)]; ok {
		return m
	}
	return "application/octet-stream"
}

var safeExt = regexp.MustCompile(`^\.[a-z0-9]{1,8}$`)

// HandleUpload — POST /api/upload, multipart с полем "file", заголовок X-Session: <id сессии>.
func (h *Hub) HandleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := h.SessionNick(r.Header.Get("X-Session")); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.opts.MaxUploadBytes+64<<10)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			http.Error(w, "file field missing", http.StatusBadRequest)
			return
		}
		if part.FormName() != "file" {
			part.Close()
			continue
		}
		att, err := h.saveUpload(part.FileName(), part)
		part.Close()
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "upload failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(att)
		return
	}
}

func (h *Hub) saveUpload(origName string, src io.Reader) (store.Attachment, error) {
	ext := strings.ToLower(filepath.Ext(origName))
	if !safeExt.MatchString(ext) {
		ext = ""
	}
	if err := os.MkdirAll(h.opts.UploadDir, 0o755); err != nil {
		return store.Attachment{}, err
	}
	name := randomHex(12) + ext
	tmp, err := os.CreateTemp(h.opts.UploadDir, ".up-*")
	if err != nil {
		return store.Attachment{}, err
	}
	n, err := io.Copy(tmp, io.LimitReader(src, h.opts.MaxUploadBytes+1))
	cerr := tmp.Close()
	if err == nil {
		err = cerr
	}
	if err == nil && n > h.opts.MaxUploadBytes {
		err = &http.MaxBytesError{Limit: h.opts.MaxUploadBytes}
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return store.Attachment{}, err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(h.opts.UploadDir, name)); err != nil {
		_ = os.Remove(tmp.Name())
		return store.Attachment{}, err
	}
	return store.Attachment{
		URL:  "/uploads/" + name,
		Name: filepath.Base(strings.ReplaceAll(origName, "\\", "/")),
		Mime: mimeForExt(ext),
		Size: n,
	}, nil
}

// UploadsHandler раздаёт загруженные файлы с безопасными заголовками.
func (h *Hub) UploadsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		if !uploadURL.MatchString("/uploads/" + name) {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(filepath.Join(h.opts.UploadDir, name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			http.NotFound(w, r)
			return
		}
		mime := mimeForExt(filepath.Ext(name))
		w.Header().Set("Content-Type", mime)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		if mime == "application/octet-stream" || r.URL.Query().Get("download") != "" {
			dl := r.URL.Query().Get("name")
			if dl == "" {
				dl = name
			}
			w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+urlPathEscape(dl))
		}
		http.ServeContent(w, r, "", fi.ModTime(), f)
	})
}

func urlPathEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(hexByte(c)))
		}
	}
	return b.String()
}

func hexByte(c byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[c>>4], digits[c&15]})
}
