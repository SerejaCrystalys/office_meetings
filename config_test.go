package main

import (
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
