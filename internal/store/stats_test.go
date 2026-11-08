package store

import (
	"context"
	"testing"
)

func TestVoiceStats(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	const min = 60_000
	base := int64(1_000_000_000)
	add := func(nick string, ch int64, name string, from, to int64) int64 {
		id, err := s.VoiceStart(ctx, nick, ch, name, base+from)
		if err != nil {
			t.Fatal(err)
		}
		if to >= 0 {
			if err := s.VoiceEnd(ctx, id, base+to); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	add("Вася", 1, "Общий", 0, 30*min)
	add("вася", 2, "Катка", 40*min, 50*min)
	add("Петя", 1, "Общий", 10*min, 20*min)
	add("Петя", 1, "Общий", 21*min, 21*min+2000) // 2 с — не считается и удаляется
	live := add("Коля", 2, "Катка", 55*min, -1)  // ещё сидит

	st, err := s.VoiceStats(ctx, 0, base+60*min, map[int64]bool{live: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Users) != 3 || st.Users[0].Nickname != "вася" || st.Users[0].Ms != 40*min || st.Users[0].Sessions != 2 {
		t.Fatalf("люди: %+v", st.Users)
	}
	if c := st.Users[0].Channels; len(c) != 2 || c[0].Name != "Общий" || c[0].Ms != 30*min {
		t.Fatalf("каналы Васи: %+v", c)
	}
	if st.Users[1].Nickname != "Петя" || st.Users[1].Ms != 10*min || st.Users[1].Sessions != 1 {
		t.Fatalf("Петя: %+v", st.Users[1])
	}
	if st.Users[2].Nickname != "Коля" || st.Users[2].Ms != 5*min {
		t.Fatalf("Коля (идёт сейчас): %+v", st.Users[2])
	}
	if st.TotalMs != 55*min || len(st.Channels) != 2 || st.Channels[0].Name != "Общий" || st.Channels[0].People != 2 {
		t.Fatalf("каналы: %+v total %d", st.Channels, st.TotalMs)
	}

	// за последние 15 минут: у Васи только кусок «Катки», сессия Пети раньше
	st, _ = s.VoiceStats(ctx, base+45*min, base+60*min, map[int64]bool{live: true})
	if len(st.Users) != 2 || st.Users[0].Ms != 5*min || st.Users[1].Ms != 5*min {
		t.Fatalf("за период: %+v", st.Users)
	}
}

func TestAIMemoryCap(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	for i := 0; i < aiMemPerNick+3; i++ {
		if err := s.AIRemember(ctx, "Вася", "факт "+string(rune('a'+i)), "Петя"); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.AIRemember(ctx, "Петя", "любит пиво", "Петя")
	all, _ := s.AIMemories(ctx, 100)
	if len(all) != aiMemPerNick+1 || all[0].Nickname != "Петя" {
		t.Fatalf("память: %d %+v", len(all), all[0])
	}
	_ = s.AIForget(ctx, "ВАСЯ")
	all, _ = s.AIMemories(ctx, 100)
	if len(all) != 1 {
		t.Fatalf("после забыть: %+v", all)
	}
}
