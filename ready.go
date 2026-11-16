package main

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	webview2 "github.com/jchv/go-webview2"
)

//go:embed ready.html
var readyHTML string

type callState struct {
	ID        string
	Title     string
	Message   string
	Start     time.Time
	Deadline  time.Time
	Local     bool // проверочный, в сеть не уходит
	Responses map[string]response
	Order     []string
}

type uiPerson struct {
	Name   string `json:"name"`
	Answer string `json:"answer"`
	Me     bool   `json:"me"`
}

type uiState struct {
	Title      string     `json:"title"`
	Message    string     `json:"message"`
	StartMs    int64      `json:"startMs"`
	DeadlineMs int64      `json:"deadlineMs"`
	NowMs      int64      `json:"nowMs"`
	Mine       string     `json:"mine"`
	People     []uiPerson `json:"people"`
	Accepted   int        `json:"accepted"`
	Declined   int        `json:"declined"`
}

var (
	callsMu  sync.Mutex
	calls    = map[string]*callState{}
	pending  = map[string][]response{} // ответы, пришедшие раньше самого сбора
	clientID string

	winMu   sync.Mutex
	curWin  webview2.WebView
	curCall string
)

// Новый сбор (от кнопки или из чьего-то трея).
func onCall(cfg Config, ev ntfyEvent) {
	title := ev.Title
	if title == "" {
		title = "Сбор"
	}
	start := time.Unix(ev.Time, 0)
	st := &callState{
		ID:        ev.ID,
		Title:     title,
		Message:   ev.Message,
		Start:     start,
		Deadline:  start.Add(time.Duration(cfg.ReadyTimeout) * time.Second),
		Responses: map[string]response{},
	}

	callsMu.Lock()
	for id, c := range calls { // подчищаем старые
		if time.Since(c.Start) > 15*time.Minute {
			delete(calls, id)
		}
	}
	if _, dup := calls[ev.ID]; dup {
		callsMu.Unlock()
		return
	}
	calls[ev.ID] = st
	for _, r := range pending[ev.ID] {
		addResponse(st, r)
	}
	delete(pending, ev.ID)
	callsMu.Unlock()

	setLast("Последний: " + title + " в " + start.Format("15:04"))
	showReady(cfg, ev.ID)
}

// Локальная проверка окна — никому не отправляется.
func testCall(cfg Config) {
	id := "local-" + time.Now().Format("150405.000")
	now := time.Now()
	st := &callState{
		ID: id, Title: "Проверка", Message: "Так будет выглядеть сбор",
		Start: now, Deadline: now.Add(time.Duration(cfg.ReadyTimeout) * time.Second),
		Local: true, Responses: map[string]response{},
	}
	addResponse(st, response{Call: id, Client: "demo1", Name: "Вася", Answer: "accept"})
	addResponse(st, response{Call: id, Client: "demo2", Name: "Петя", Answer: "decline"})
	callsMu.Lock()
	calls[id] = st
	callsMu.Unlock()
	showReady(cfg, id)
}

func addResponse(st *callState, r response) {
	if _, ok := st.Responses[r.Client]; !ok {
		st.Order = append(st.Order, r.Client)
	}
	st.Responses[r.Client] = r
}

func onResponse(r response) {
	callsMu.Lock()
	st, ok := calls[r.Call]
	if ok {
		addResponse(st, r)
	} else if len(pending) < 100 {
		pending[r.Call] = append(pending[r.Call], r)
	}
	callsMu.Unlock()
	if ok {
		refresh(r.Call)
	}
}

func snapshot(id string) uiState {
	callsMu.Lock()
	defer callsMu.Unlock()
	st, ok := calls[id]
	if !ok {
		return uiState{Title: "Сбор", NowMs: time.Now().UnixMilli()}
	}
	s := uiState{
		Title:      st.Title,
		Message:    st.Message,
		StartMs:    st.Start.UnixMilli(),
		DeadlineMs: st.Deadline.UnixMilli(),
		NowMs:      time.Now().UnixMilli(),
		People:     []uiPerson{},
	}
	for _, c := range st.Order {
		r := st.Responses[c]
		me := c == clientID
		if me {
			s.Mine = r.Answer
		}
		s.People = append(s.People, uiPerson{Name: r.Name, Answer: r.Answer, Me: me})
		if r.Answer == "accept" {
			s.Accepted++
		} else {
			s.Declined++
		}
	}
	// принявшие сверху, ты — первым в своей группе
	sort.SliceStable(s.People, func(i, j int) bool {
		a, b := s.People[i], s.People[j]
		if a.Answer != b.Answer {
			return a.Answer == "accept"
		}
		return a.Me && !b.Me
	})
	return s
}

func answer(cfg Config, id, ans string) uiState {
	if ans != "accept" && ans != "decline" {
		return snapshot(id)
	}
	callsMu.Lock()
	st, ok := calls[id]
	var r response
	send := false
	if ok && time.Now().Before(st.Deadline) {
		if _, already := st.Responses[clientID]; !already {
			r = response{Call: id, Client: clientID, Name: cfg.Name, Answer: ans}
			addResponse(st, r)
			send = !st.Local
		}
	}
	callsMu.Unlock()

	if send {
		go func() {
			if err := sendResponse(cfg, r); err != nil {
				messageBox("Office Meetings", "Ответ не ушёл: "+err.Error(), mbOK|mbIconError)
			}
		}()
	}
	return snapshot(id)
}

func refresh(id string) {
	winMu.Lock()
	w, cur := curWin, curCall
	winMu.Unlock()
	if w == nil || cur != id {
		return
	}
	b, _ := json.Marshal(snapshot(id))
	js := "window.render && render(" + string(b) + ")"
	w.Dispatch(func() { w.Eval(js) })
}

func showReady(cfg Config, id string) {
	beep()
	winMu.Lock()
	if curWin != nil { // новый сбор вытесняет старое окно
		old := curWin
		old.Dispatch(func() { old.Terminate() })
		curWin, curCall = nil, ""
	}
	winMu.Unlock()
	go runWindow(cfg, id)
}

func webviewDataPath() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "OfficeMeetings", "WebView2")
}

func runWindow(cfg Config, id string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	st := snapshot(id)
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  webviewDataPath(),
		WindowOptions: webview2.WindowOptions{
			Title:  "Сбор: " + st.Title,
			Width:  440,
			Height: 580,
			Center: true,
		},
	})
	if w == nil { // нет WebView2 — показываем простое окно
		notify(st.Title, strings.TrimSpace(st.Message))
		return
	}
	defer w.Destroy()
	w.SetSize(440, 580, webview2.HintFixed)

	_ = w.Bind("getState", func() uiState { return snapshot(id) })
	_ = w.Bind("answer", func(a string) uiState { return answer(cfg, id, a) })
	_ = w.Bind("closeWindow", func() { w.Dispatch(func() { w.Terminate() }) })
	w.SetHtml(readyHTML)

	winMu.Lock()
	curWin, curCall = w, id
	winMu.Unlock()

	bringToFront(uintptr(w.Window()))
	w.Run()

	winMu.Lock()
	if curWin == w {
		curWin, curCall = nil, ""
	}
	winMu.Unlock()
}
