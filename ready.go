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
	From      string // кто позвал; "" — кнопка
	Start     time.Time
	Deadline  time.Time
	Local     bool // проверочный, в сеть не уходит
	Responses map[string]response
	Order     []string
}

type uiPerson struct {
	Name   string `json:"name"`
	Answer string `json:"answer"`
	Reason string `json:"reason,omitempty"`
	Me     bool   `json:"me"`
}

type uiState struct {
	Title      string     `json:"title"`
	Message    string     `json:"message"`
	From       string     `json:"from"` // кто позвал; "" — кнопка
	StartMs    int64      `json:"startMs"`
	DeadlineMs int64      `json:"deadlineMs"`
	NowMs      int64      `json:"nowMs"`
	Mine       string     `json:"mine"`
	People     []uiPerson `json:"people"`
	Accepted   int        `json:"accepted"`
	Declined   int        `json:"declined"`
	Waiting    int        `json:"waiting"` // получили сбор, но молчат
	Reasons    []string   `json:"reasons"` // подсказки причины отказа
}

var (
	callsMu  sync.Mutex
	calls    = map[string]*callState{}
	pending  = map[string][]response{} // ответы, пришедшие раньше самого сбора
	myCalls  = map[string]time.Time{}  // сборы, которые позвали мы (id → когда)
	clientID string

	declineReasons = defaultDeclineReasons // из config.json, задаётся в main

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
		From:      ev.initiator(),
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
	for id, t := range myCalls {
		if time.Since(t) > 15*time.Minute {
			delete(myCalls, id)
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
	// Сообщаем остальным, что сбор до нас дошёл: пока не нажмём кнопку,
	// у всех в окне мы будем в списке «молчат». Свой сбор уже принят в startCall.
	var seen response
	_, mine := myCalls[ev.ID]
	sendSeen := !mine && time.Now().Before(st.Deadline)
	if mine {
		addResponse(st, response{Call: ev.ID, Client: clientID, Name: cfg.Name, Answer: ansAccept})
	} else if sendSeen {
		seen = response{Call: ev.ID, Client: clientID, Name: cfg.Name, Answer: ansSeen}
		addResponse(st, seen)
	}
	callsMu.Unlock()

	if sendSeen {
		go func() { _ = sendResponse(cfg, seen) }() // не дошло — не страшно, ошибку не показываем
	}
	who := st.From
	if who == "" {
		who = "кнопка"
	}
	setLast("Последний: " + title + " в " + start.Format("15:04") + " — " + who)
	showReady(cfg, ev.ID)
}

// startCall — позвать всех из трея. Инициатор сразу считается принявшим, как в Доте.
func startCall(cfg Config, a Activity) error {
	id, err := publishCall(cfg, a)
	if err != nil {
		return err
	}
	if id == "" {
		return nil // сбор ушёл, но id не знаем — автопринятие пропускаем
	}
	r := response{Call: id, Client: clientID, Name: cfg.Name, Answer: ansAccept}
	callsMu.Lock()
	myCalls[id] = time.Now()
	st, ok := calls[id] // эхо сбора могло прийти раньше ответа на POST
	if ok {
		addResponse(st, r)
	}
	callsMu.Unlock()
	if ok {
		refresh(id)
	}
	go func() { _ = sendResponse(cfg, r) }()
	return nil
}

// publishCall отправляет сбор. Сеть может оборвать соединение уже после того, как сбор
// дошёл до ntfy, поэтому вслепую не повторяем: ждём немного эхо из подписки, и только
// если его нет — пробуем ещё раз.
func publishCall(cfg Config, a Activity) (string, error) {
	sent := time.Now()
	id, err := publish(cfg, a)
	if err == nil || !retryable(err) {
		return id, err
	}
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		if id := findMyCall(cfg, a, sent); id != "" {
			return id, nil
		}
	}
	return publish(cfg, a)
}

// findMyCall — пришёл ли из подписки наш сбор (тот же заголовок, текст, наше имя) после sent.
func findMyCall(cfg Config, a Activity, sent time.Time) string {
	callsMu.Lock()
	defer callsMu.Unlock()
	for id, c := range calls {
		if !c.Local && c.Title == a.Title && c.Message == a.Message && c.From == cleanName(cfg.Name) &&
			!c.Start.Before(sent.Add(-2*time.Second)) {
			return id
		}
	}
	return ""
}

// Локальная проверка окна — никому не отправляется.
func testCall(cfg Config) {
	id := "local-" + time.Now().Format("150405.000")
	now := time.Now()
	st := &callState{
		ID: id, Title: "Проверка", Message: "Так будет выглядеть сбор", From: "Вася",
		Start: now, Deadline: now.Add(time.Duration(cfg.ReadyTimeout) * time.Second),
		Local: true, Responses: map[string]response{},
	}
	addResponse(st, response{Call: id, Client: "demo1", Name: "Вася", Answer: ansAccept})
	addResponse(st, response{Call: id, Client: "demo2", Name: "Петя", Answer: ansDecline, Reason: "На созвоне до 15:00"})
	addResponse(st, response{Call: id, Client: "demo3", Name: "Коля", Answer: ansSeen})
	addResponse(st, response{Call: id, Client: clientID, Name: cfg.Name, Answer: ansSeen})
	callsMu.Lock()
	calls[id] = st
	callsMu.Unlock()
	showReady(cfg, id)
}

func addResponse(st *callState, r response) {
	old, ok := st.Responses[r.Client]
	if !ok {
		st.Order = append(st.Order, r.Client)
	} else if r.Answer == ansSeen && old.Answer != ansSeen {
		return // «получил» не перетирает уже нажатый ответ (если пришёл позже него)
	}
	st.Responses[r.Client] = r
}

// answered — человек уже нажал «Принять» или «Отклонить».
func answered(st *callState, client string) bool {
	r, ok := st.Responses[client]
	return ok && r.Answer != ansSeen
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
		return uiState{Title: "Сбор", NowMs: time.Now().UnixMilli(), People: []uiPerson{}, Reasons: declineReasons}
	}
	s := uiState{
		Title:      st.Title,
		Message:    st.Message,
		From:       st.From,
		StartMs:    st.Start.UnixMilli(),
		DeadlineMs: st.Deadline.UnixMilli(),
		NowMs:      time.Now().UnixMilli(),
		People:     []uiPerson{},
		Reasons:    declineReasons,
	}
	for _, c := range st.Order {
		r := st.Responses[c]
		me := c == clientID
		if me && r.Answer != ansSeen {
			s.Mine = r.Answer
		}
		s.People = append(s.People, uiPerson{Name: r.Name, Answer: r.Answer, Reason: r.Reason, Me: me})
		switch r.Answer {
		case ansAccept:
			s.Accepted++
		case ansDecline:
			s.Declined++
		default:
			s.Waiting++
		}
	}
	// сверху принявшие, потом молчащие, внизу отказавшиеся; ты — первым в своей группе
	rank := map[string]int{ansAccept: 0, ansSeen: 1, ansDecline: 2}
	sort.SliceStable(s.People, func(i, j int) bool {
		a, b := s.People[i], s.People[j]
		if rank[a.Answer] != rank[b.Answer] {
			return rank[a.Answer] < rank[b.Answer]
		}
		return a.Me && !b.Me
	})
	return s
}

// answer — человек нажал кнопку; reason учитывается только для отказа.
func answer(cfg Config, id, ans, reason string) uiState {
	if ans != ansAccept && ans != ansDecline {
		return snapshot(id)
	}
	reason = cleanReason(reason)
	if ans != ansDecline {
		reason = ""
	}
	callsMu.Lock()
	st, ok := calls[id]
	var r response
	send := false
	if ok && time.Now().Before(st.Deadline) {
		if !answered(st, clientID) {
			r = response{Call: id, Client: clientID, Name: cfg.Name, Answer: ans, Reason: reason}
			addResponse(st, r)
			send = !st.Local
		}
	}
	callsMu.Unlock()

	if send {
		go func() {
			if err := sendResponse(cfg, r); err != nil {
				revertAnswer(id, r)
				refresh(id)
				messageBox("Office Meetings", "Ответ не ушёл: "+err.Error()+"\n\nНажми «Принять» или «Отклонить» ещё раз.", mbOK|mbIconError)
			}
		}()
	}
	return snapshot(id)
}

// revertAnswer — ответ так и не ушёл: снова «молчим», чтобы кнопки в окне ожили.
// Если ответ на самом деле дошёл (оборвалось уже после), его эхо из ntfy вернёт всё как было.
func revertAnswer(id string, r response) {
	callsMu.Lock()
	defer callsMu.Unlock()
	if st, ok := calls[id]; ok && st.Responses[r.Client] == r {
		r.Answer = ansSeen
		st.Responses[r.Client] = r
	}
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
	_ = w.Bind("answer", func(a, reason string) uiState { return answer(cfg, id, a, reason) })
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
