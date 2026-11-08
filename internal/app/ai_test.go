package app

import "testing"

func TestIsAICall(t *testing.T) {
	for text, want := range map[string]bool{
		"/ai как дела":         true,
		"/AI":                  true,
		"  /ai\nвопрос":        true,
		"@ai помоги":           true,
		"эй @AI, скажи":        true,
		"спроси @ai":           true,
		"/aim не про ИИ":       false,
		"почта vasya@ai.ru":    false,
		"@aidar привет":        false,
		"просто текст про ai":  false,
		"https://x.ru/ai/test": false,
	} {
		if got := isAICall(text); got != want {
			t.Errorf("isAICall(%q) = %v, want %v", text, got, want)
		}
	}
}
