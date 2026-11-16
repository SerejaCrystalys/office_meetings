package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("wsarecv: connection forcibly closed"), true},
		{statusError(502), true},
		{statusError(400), false},
		{statusError(429), false},
	}
	for _, c := range cases {
		if got := retryable(c.err); got != c.want {
			t.Errorf("retryable(%v) = %v, ждали %v", c.err, got, c.want)
		}
	}
}

// Сервер, который первые fails запросов обрывает соединение или отвечает 503.
func flakyServer(t *testing.T, fails int32, mode string) (*httptest.Server, *int32) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) <= fails {
			if mode == "reset" {
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestSendResponseRetries(t *testing.T) {
	old := retryPauses
	retryPauses = []time.Duration{0, 0, 0}
	t.Cleanup(func() { retryPauses = old })
	r := response{Call: "c", Client: "me", Name: "Я", Answer: ansAccept}

	for _, mode := range []string{"reset", "503"} {
		srv, n := flakyServer(t, 2, mode)
		if err := sendResponse(Config{Server: srv.URL, Topic: "t"}, r); err != nil {
			t.Errorf("%s: после двух сбоев третья попытка должна пройти, а ошибка: %v", mode, err)
		}
		if *n != 3 {
			t.Errorf("%s: ждали 3 попытки, было %d", mode, *n)
		}
	}

	srv, n := flakyServer(t, 10, "reset")
	if err := sendResponse(Config{Server: srv.URL, Topic: "t"}, r); err == nil {
		t.Error("сервер всё время рвёт соединение — должна быть ошибка")
	}
	if *n != int32(len(retryPauses)) {
		t.Errorf("ждали %d попытки, было %d", len(retryPauses), *n)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(bad.Close)
	if err := sendResponse(Config{Server: bad.URL, Topic: "t"}, r); err == nil || retryable(err) {
		t.Errorf("400 не повторяется и возвращается как ошибка, а получили %v", err)
	}
}

func TestRevertAnswer(t *testing.T) {
	clientID = "me"
	st := newTestCall("t-revert")
	addResponse(st, response{Call: st.ID, Client: "me", Name: "Я", Answer: ansSeen})
	s := answer(Config{Name: "Я"}, st.ID, ansAccept, "", "") // Local — в сеть не уходит
	if s.Mine != ansAccept {
		t.Fatalf("ждали accept, получили %q", s.Mine)
	}
	revertAnswer(st.ID, response{Call: st.ID, Client: "me", Name: "Я", Answer: ansAccept}, response{})
	if s = snapshot(st.ID); s.Mine != "" || s.Waiting != 1 {
		t.Fatalf("после отката ждали «молчит» и пустой Mine, получили Mine=%q waiting=%d", s.Mine, s.Waiting)
	}
	// можно ответить ещё раз
	if s = answer(Config{Name: "Я"}, st.ID, ansDecline, "", ""); s.Mine != ansDecline {
		t.Fatalf("после отката ответ должен приниматься снова, получили %q", s.Mine)
	}
}

func TestInitiatorTag(t *testing.T) {
	if got := fromTag("  Серёга,  Петров \n"); got != "from:Серёга  Петров" && got != "from:Серёга Петров" {
		t.Errorf("тег инициатора: %q", got)
	}
	ev := ntfyEvent{Tags: []string{"warning", "from:Серёга"}}
	if got := ev.initiator(); got != "Серёга" {
		t.Errorf("ждали «Серёга», получили %q", got)
	}
	if got := (ntfyEvent{}).initiator(); got != "" {
		t.Errorf("сбор с кнопки без тега — ждали пусто, получили %q", got)
	}
}

// Обрыв после того, как сбор дошёл: эхо пришло — второй раз не отправляем.
func TestPublishCallNoDuplicate(t *testing.T) {
	srv, n := flakyServer(t, 1, "reset")
	cfg := Config{Server: srv.URL, Topic: "t", Name: "Я"}
	a := Activity{Title: "Курилка", Message: "Сбор!"}
	go func() {
		time.Sleep(700 * time.Millisecond)
		callsMu.Lock()
		calls["echo-1"] = &callState{ID: "echo-1", Title: a.Title, Message: a.Message, From: "Я",
			Start: time.Now(), Responses: map[string]response{}}
		callsMu.Unlock()
	}()
	id, err := publishCall(cfg, a)
	if err != nil || id != "echo-1" {
		t.Fatalf("ждали id эха echo-1 без ошибки, получили %q, %v", id, err)
	}
	if *n != 1 {
		t.Fatalf("сбор не должен отправляться повторно, запросов было %d", *n)
	}
}

// Инициатор после отправки сразу числится принявшим.
func TestStartCallAutoAccept(t *testing.T) {
	clientID = "me"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"call-42","event":"message"}`))
	}))
	t.Cleanup(srv.Close)
	st := &callState{ID: "call-42", Title: "Курилка", Deadline: time.Now().Add(time.Minute), Responses: map[string]response{}}
	callsMu.Lock()
	calls["call-42"] = st // эхо пришло раньше ответа на POST
	callsMu.Unlock()
	if err := startCall(Config{Server: srv.URL, Topic: "t", Name: "Я"}, Activity{Title: "Курилка"}); err != nil {
		t.Fatal(err)
	}
	if s := snapshot("call-42"); s.Mine != ansAccept {
		t.Fatalf("инициатор должен быть принявшим, Mine=%q", s.Mine)
	}
	callsMu.Lock()
	_, ok := myCalls["call-42"]
	callsMu.Unlock()
	if !ok {
		t.Fatal("сбор должен запомниться как свой")
	}
}
