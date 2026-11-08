package store

import (
	"context"
	"testing"
)

func TestPinsPollsSearch(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	ch, err := s.CreateChannel(ctx, "text", "тест")
	if err != nil {
		t.Fatal(err)
	}
	m1, _ := s.CreateMessage(ctx, ch.ID, "Вася", "Сервер CS: 1.2.3.4:27015", 0, nil)
	_, _ = s.CreateMessage(ctx, ch.ID, "Петя", "привет всем", 0, nil)

	// закреп
	p, err := s.SetPinned(ctx, m1.ID, "Петя", true)
	if err != nil || p.Pinned == nil || p.Pinned.By != "Петя" {
		t.Fatalf("закреп: %+v %v", p.Pinned, err)
	}
	pins, _ := s.Pinned(ctx, ch.ID)
	if len(pins) != 1 || pins[0].ID != m1.ID {
		t.Fatalf("список закрепов: %+v", pins)
	}
	p, _ = s.SetPinned(ctx, m1.ID, "Петя", false)
	if p.Pinned != nil {
		t.Fatal("не открепилось")
	}

	// опрос с одним ответом: повторный голос переносится
	if _, err := s.CreatePoll(ctx, ch.ID, "Вася", "во что?", []string{"CS", "cs", ""}, false); err != ErrBadPoll {
		t.Fatalf("одинаковые варианты должны отсекаться: %v", err)
	}
	poll, err := s.CreatePoll(ctx, ch.ID, "Вася", "Во что катаем?", []string{"CS2", "Дота", "Спать"}, false)
	if err != nil || poll.Poll == nil || len(poll.Poll.Options) != 3 {
		t.Fatalf("опрос: %+v %v", poll.Poll, err)
	}
	_, _ = s.VotePoll(ctx, poll.ID, 0, "Петя")
	v, _ := s.VotePoll(ctx, poll.ID, 1, "петя")
	if len(v.Poll.Options[0].Votes) != 0 || len(v.Poll.Options[1].Votes) != 1 {
		t.Fatalf("один ответ: %+v", v.Poll.Options)
	}
	v, _ = s.VotePoll(ctx, poll.ID, 1, "Петя") // повторный клик — снять голос
	if len(v.Poll.Options[1].Votes) != 0 {
		t.Fatalf("снятие голоса: %+v", v.Poll.Options)
	}
	multi, _ := s.CreatePoll(ctx, ch.ID, "Вася", "Когда?", []string{"сб", "вс"}, true)
	_, _ = s.VotePoll(ctx, multi.ID, 0, "Петя")
	v, _ = s.VotePoll(ctx, multi.ID, 1, "Петя")
	if len(v.Poll.Options[0].Votes) != 1 || len(v.Poll.Options[1].Votes) != 1 {
		t.Fatalf("несколько ответов: %+v", v.Poll.Options)
	}
	if _, err := s.VotePoll(ctx, m1.ID, 0, "Петя"); err != ErrBadPoll {
		t.Fatal("голос не в опросе")
	}
	// голоса видны и в истории
	hist, _, _ := s.History(ctx, ch.ID, 0, 10)
	if last := hist[len(hist)-1]; last.Poll == nil || len(last.Poll.Options[0].Votes) != 1 {
		t.Fatalf("история: %+v", last.Poll)
	}

	// поиск: без регистра, по кириллице, по автору, по опросам
	if r, _ := s.Search(ctx, []int64{ch.ID}, "ПРИВЕТ", "", 10); len(r) != 1 || r[0].Author != "Петя" {
		t.Fatalf("поиск по тексту: %+v", r)
	}
	if r, _ := s.Search(ctx, []int64{ch.ID}, "", "вася", 10); len(r) != 3 {
		t.Fatalf("поиск по автору: %d", len(r))
	}
	if r, _ := s.Search(ctx, []int64{ch.ID}, "катаем", "", 10); len(r) != 1 || r[0].Poll == nil {
		t.Fatalf("поиск по опросу: %+v", r)
	}
	if r, _ := s.Search(ctx, []int64{999}, "привет", "", 10); len(r) != 0 {
		t.Fatal("чужой канал")
	}

	// превью: после правки текста старые не записываются
	if _, ok, _ := s.SetEmbeds(ctx, m1.ID, "другой текст", []Embed{{URL: "x"}}); ok {
		t.Fatal("превью к изменённому тексту")
	}
	e, ok, _ := s.SetEmbeds(ctx, m1.ID, m1.Text, []Embed{{URL: "https://a.b", Title: "T"}})
	if !ok || len(e.Embeds) != 1 {
		t.Fatalf("превью: %+v", e.Embeds)
	}
	ed, _ := s.EditMessage(ctx, m1.ID, "Вася", "новый текст")
	if len(ed.Embeds) != 0 {
		t.Fatal("после правки превью должны сброситься")
	}
}
