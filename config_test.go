package main

import (
	"os"
	"strings"
	"testing"
)

func TestRespTopic(t *testing.T) {
	for _, n := range []int{1, 10, 61, 62, 63, 64} {
		topic := strings.Repeat("a", n)
		got := Config{Topic: topic}.RespTopic()
		if len(got) > maxTopicLen {
			t.Errorf("топик %d символов: топик ответов %d символов, больше лимита ntfy", n, len(got))
		}
		if !topicRe.MatchString(got) {
			t.Errorf("топик %d символов: топик ответов %q не проходит проверку ntfy", n, got)
		}
		if got == topic {
			t.Errorf("топик %d символов: топик ответов совпал с основным", n)
		}
		if n <= 62 && got != topic+"-r" {
			t.Errorf("топик %d символов: ждали %q, получили %q (короткие топики не должны меняться)", n, topic+"-r", got)
		}
	}
}

func TestTopicRe(t *testing.T) {
	ok := []string{"knopka-abc_DEF-123", strings.Repeat("x", 64)}
	bad := []string{"", strings.Repeat("x", 65), "с кириллицей", "with space", "a/b", "a,b"}
	for _, s := range ok {
		if !topicRe.MatchString(s) {
			t.Errorf("%q должен быть допустимым топиком", s)
		}
	}
	for _, s := range bad {
		if topicRe.MatchString(s) {
			t.Errorf("%q не должен быть допустимым топиком", s)
		}
	}
}

func TestCleanReason(t *testing.T) {
	if got := cleanReason("  На   созвоне\nдо 15:00 \t"); got != "На созвоне до 15:00" {
		t.Errorf("пробелы и переводы строк: %q", got)
	}
	long := strings.Repeat("я", 200)
	if got := []rune(cleanReason(long)); len(got) != maxReasonLen {
		t.Errorf("длинная причина должна обрезаться до %d символов, а вышло %d", maxReasonLen, len(got))
	}
}

func TestDeclineReasonsConfig(t *testing.T) {
	dir := t.TempDir()
	load := func(body string) Config {
		t.Helper()
		p := dir + "/config.json"
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfigFrom(p)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	if cfg := load(`{"topic":"t"}`); len(cfg.DeclineReasons) != len(defaultDeclineReasons) {
		t.Errorf("нет ключа — ждали стандартный список, получили %v", cfg.DeclineReasons)
	}
	if cfg := load(`{"topic":"t","decline_reasons":[]}`); len(cfg.DeclineReasons) != 0 {
		t.Errorf("пустой список — ждали без подсказок, получили %v", cfg.DeclineReasons)
	}
	cfg := load(`{"topic":"t","decline_reasons":["  Болею ", "", "1","2","3","4","5","6","7","8"]}`)
	if len(cfg.DeclineReasons) != maxDeclineReasons || cfg.DeclineReasons[0] != "Болею" {
		t.Errorf("ждали %d подсказок, первая «Болею», получили %v", maxDeclineReasons, cfg.DeclineReasons)
	}
}
