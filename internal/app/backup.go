package app

import (
	"archive/zip"
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"zalupa228local/internal/store"
)

// Резервные копии: раз в сутки снимок базы в <data>/backups (хранится keep последних)
// и выгрузка полного архива (база + загруженные файлы) админу по одноразовой ссылке.

// StartBackups запускает ежедневные копии базы.
func StartBackups(st *store.Store, dir string, keep int) {
	go func() {
		time.Sleep(time.Minute)
		for {
			if err := dailyBackup(st, dir, keep); err != nil {
				log.Printf("бэкап базы: %v", err)
			}
			// раз в час проверяем, есть ли копия за сегодня — так переживаем перезапуски
			time.Sleep(time.Hour)
		}
	}()
}

func dailyBackup(st *store.Store, dir string, keep int) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	final := filepath.Join(dir, "z228-"+time.Now().Format("2006-01-02")+".db")
	if _, err := os.Stat(final); err != nil {
		tmp := final + ".tmp"
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := st.Snapshot(ctx, tmp); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, final); err != nil {
			return err
		}
		log.Printf("бэкап базы: %s", final)
	}
	// удаляем старые, оставляем keep самых свежих (имена с датой сортируются по времени)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "z228-") && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, n := range names[min(keep, len(names)):] {
		_ = os.Remove(filepath.Join(dir, n))
	}
	return nil
}

var backupTokens = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

// backupToken — одноразовая ссылка на скачивание архива, живёт минуту.
func (h *Hub) backupToken() string {
	t := randomHex(16)
	backupTokens.Lock()
	now := time.Now()
	for k, exp := range backupTokens.m {
		if now.After(exp) {
			delete(backupTokens.m, k)
		}
	}
	backupTokens.m[t] = now.Add(time.Minute)
	backupTokens.Unlock()
	return t
}

// HandleBackup — GET /api/backup?t=<токен>: zip с базой и всеми загрузками.
func (h *Hub) HandleBackup(w http.ResponseWriter, r *http.Request) {
	t := r.URL.Query().Get("t")
	backupTokens.Lock()
	exp, ok := backupTokens.m[t]
	delete(backupTokens.m, t)
	backupTokens.Unlock()
	if t == "" || !ok || time.Now().After(exp) {
		http.Error(w, "ссылка устарела — нажмите «Скачать» ещё раз", http.StatusForbidden)
		return
	}

	tmp := filepath.Join(h.opts.DataDir, ".backup-download.db")
	if err := h.opts.Store.Snapshot(r.Context(), tmp); err != nil {
		log.Printf("бэкап: %v", err)
		http.Error(w, "не удалось сделать копию", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp)

	name := "zalupa228-backup-" + time.Now().Format("2006-01-02-1504") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	zw := zip.NewWriter(w)
	addFile := func(src, dst string) error {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		out, err := zw.Create(dst)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, f)
		return err
	}
	if err := addFile(tmp, "z228.db"); err != nil {
		log.Printf("бэкап: %v", err)
		return
	}
	if entries, err := os.ReadDir(h.opts.UploadDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if err := addFile(filepath.Join(h.opts.UploadDir, e.Name()), "uploads/"+e.Name()); err != nil {
				log.Printf("бэкап: %v", err)
				return
			}
		}
	}
	if err := zw.Close(); err != nil {
		log.Printf("бэкап: %v", err)
		return
	}
	log.Printf("скачан полный бэкап (%s)", name)
}
