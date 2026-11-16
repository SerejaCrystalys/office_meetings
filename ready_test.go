package main

import (
	"testing"
	"time"
)

func newTestCall(id string) *callState {
	now := time.Now()
	st := &callState{ID: id, Title: "Тест", Start: now, Deadline: now.Add(time.Minute),
		Local: true, Responses: map[string]response{}}
	callsMu.Lock()
	calls[id] = st
	callsMu.Unlock()
	return st
}

// «Получил» не должен перетирать уже нажатый ответ, а ответ перетирает «получил».
func TestSeenDoesNotOverrideAnswer(t *testing.T) {
	st := newTestCall("t-seen")
	addResponse(st, response{Call: st.ID, Client: "a", Name: "А", Answer: ansSeen})
	addResponse(st, response{Call: st.ID, Client: "a", Name: "А", Answer: ansAccept})
	addResponse(st, response{Call: st.ID, Client: "a", Name: "А", Answer: ansSeen}) // эхо seen пришло позже
	if got := st.Responses["a"].Answer; got != ansAccept {
		t.Fatalf("ждали accept, получили %q", got)
	}
	if len(st.Order) != 1 {
		t.Fatalf("клиент должен быть в списке один раз, а не %d", len(st.Order))
	}
}

// Свой seen не мешает нажать «Принять».
func TestAnswerAfterSeen(t *testing.T) {
	clientID = "me"
	st := newTestCall("t-answer")
	addResponse(st, response{Call: st.ID, Client: "me", Name: "Я", Answer: ansSeen})
	s := answer(Config{Name: "Я"}, st.ID, ansAccept)
	if s.Mine != ansAccept {
		t.Fatalf("ждали Mine=accept, получили %q", s.Mine)
	}
	// второй раз ответить нельзя
	s = answer(Config{Name: "Я"}, st.ID, ansDecline)
	if s.Mine != ansAccept {
		t.Fatalf("ответ не должен меняться, получили %q", s.Mine)
	}
}

func TestSnapshotGroups(t *testing.T) {
	clientID = "me"
	st := newTestCall("t-snap")
	addResponse(st, response{Call: st.ID, Client: "d", Name: "Отказник", Answer: ansDecline})
	addResponse(st, response{Call: st.ID, Client: "w", Name: "Молчун", Answer: ansSeen})
	addResponse(st, response{Call: st.ID, Client: "me", Name: "Я", Answer: ansSeen})
	addResponse(st, response{Call: st.ID, Client: "a", Name: "Согласный", Answer: ansAccept})

	s := snapshot(st.ID)
	if s.Accepted != 1 || s.Waiting != 2 || s.Declined != 1 {
		t.Fatalf("счётчики: accepted=%d waiting=%d declined=%d", s.Accepted, s.Waiting, s.Declined)
	}
	if s.Mine != "" {
		t.Fatalf("seen не считается ответом, а Mine=%q", s.Mine)
	}
	want := []string{"Согласный", "Я", "Молчун", "Отказник"}
	for i, p := range s.People {
		if p.Name != want[i] {
			t.Fatalf("порядок: позиция %d — %q, ждали %q", i, p.Name, want[i])
		}
	}
}
