package store

import (
	"context"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOneTimeAndExpiringInvites(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.CreateInvite(ctx, "once", "", "admin", 1, 0); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.UseInvite(ctx, "once", "client-a", "Вася"); r != InviteOK {
		t.Fatalf("первый вход: %v", r)
	}
	// тот же браузер переподключается сколько угодно
	if r, _ := s.UseInvite(ctx, "once", "client-a", "Вася"); r != InviteOK {
		t.Fatalf("повторный вход того же человека: %v", r)
	}
	if r, _ := s.UseInvite(ctx, "once", "client-b", "Петя"); r != InviteUsedUp {
		t.Fatalf("второй человек по разовому ключу: %v", r)
	}
	if err := s.CreateInvite(ctx, "old", "", "admin", 0, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.UseInvite(ctx, "old", "client-a", "Вася"); r != InviteExpired {
		t.Fatalf("истёкший ключ: %v", r)
	}
	if r, _ := s.UseInvite(ctx, "nope", "client-a", "Вася"); r != InviteNotFound {
		t.Fatalf("несуществующий ключ: %v", r)
	}
	inv, err := s.GetInvite(ctx, "once")
	if err != nil || inv.MaxUses != 1 || inv.People != 1 || inv.Uses != 2 {
		t.Fatalf("ключ: %+v %v", inv, err)
	}
}

func TestBansMembersDMsPerms(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	for _, n := range []string{"Вася", "Петя", "Коля"} {
		if _, err := s.Seen(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddBan(ctx, "Коля", "client-k", "admin"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		nick, client string
		want         bool
	}{{"коля", "x", true}, {"Другой", "client-k", true}, {"Вася", "client-v", false}, {"Вася", "", false}} {
		if got, _ := s.IsBanned(ctx, c.nick, c.client); got != c.want {
			t.Fatalf("IsBanned(%s,%s)=%v", c.nick, c.client, got)
		}
	}
	mem, _ := s.Members(ctx, 10)
	if len(mem) != 2 {
		t.Fatalf("забаненный в списке участников: %+v", mem)
	}
	bans, _ := s.Bans(ctx)
	if len(bans) != 1 || bans[0].Nick != "Коля" {
		t.Fatalf("баны: %+v", bans)
	}
	_ = s.RemoveBan(ctx, bans[0].ID)
	if got, _ := s.IsBanned(ctx, "Коля", "client-k"); got {
		t.Fatal("разбан не сработал")
	}

	id, err := s.OpenDM(ctx, "Вася", "Петя")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := s.OpenDM(ctx, "петя", "вася"); again != id {
		t.Fatalf("второй ЛС-канал для той же пары: %d != %d", again, id)
	}
	if _, err := s.CreateMessage(ctx, id, "Вася", "привет", 0, nil); err != nil {
		t.Fatal(err)
	}
	dms, _ := s.DMsOf(ctx, "Петя")
	if len(dms) != 1 || dms[0].Peer != "Вася" || dms[0].LastID == 0 {
		t.Fatalf("ЛС Пети: %+v", dms)
	}
	chs, _ := s.Channels(ctx)
	for _, c := range chs {
		if c.ID == id {
			t.Fatal("ЛС попал в общий список каналов")
		}
	}
	if m, _ := s.DMMembers(ctx, chs[0].ID); len(m) != 0 {
		t.Fatal("у обычного канала есть участники ЛС")
	}

	if err := s.CreateRole(ctx, "Модер", "#ffffff", []string{"kick", "ai"}); err != nil {
		t.Fatal(err)
	}
	roles, _, _ := s.Roles(ctx)
	_ = s.AssignRole(ctx, "Вася", roles[0].ID)
	if p, _ := s.PermsOf(ctx, "вася"); len(p) != 2 || p[0] != "kick" {
		t.Fatalf("права: %v", p)
	}
	_ = s.SetRolePerms(ctx, roles[0].ID, []string{"admin"})
	if p, _ := s.PermsOf(ctx, "Вася"); len(p) != 1 || p[0] != "admin" {
		t.Fatalf("права после правки: %v", p)
	}
	if p, _ := s.PermsOf(ctx, "Петя"); len(p) != 0 {
		t.Fatalf("права без роли: %v", p)
	}
}

// Старая база (без новых столбцов) должна открываться и мигрировать.
func TestMigrateTwice(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("открытие %d: %v", i, err)
		}
		s.Close()
	}
}
