package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

	Options   []string // голосование «куда идём»; пусто — сбор без голосования
	Winner    string   // что выбрал инициатор
	WinnerBy  string
	WinnerWhy string // как выбрала программа (по голосам / жребий); пусто — выбрал человек
	autoArmed bool   // таймер автовыбора к дедлайну уже поставлен
}

type uiPerson struct {
	Name   string `json:"name"`
	Answer string `json:"answer"`
	Reason string `json:"reason,omitempty"`
	Me     bool   `json:"me"`
}

// uiOption — строка голосования: место и кто за него.
type uiOption struct {
	Name   string   `json:"name"`
	Voters []string `json:"voters"`
	Mine   bool     `json:"mine"`
	Custom bool     `json:"custom"` // вписали в «Другое»
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

	Options   []uiOption `json:"options"` // пусто — без голосования
	Winner    string     `json:"winner"`
	WinnerBy  string     `json:"winnerBy"`
	WinnerWhy string     `json:"winnerWhy"` // пусто — выбрал человек
	CanPick   bool       `json:"canPick"`   // это наш сбор — нам выбирать победителя
}

var (
	callsMu  sync.Mutex
	calls    = map[string]*callState{}
	pending  = map[string][]response{} // ответы, пришедшие раньше самого сбора
	myCalls  = map[string]time.Time{}  // сборы, которые позвали мы (id → когда)
	lastCall string                    // последний сбор — его окно можно открыть из трея
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
		Options:   ev.options(),
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
	lastCall = ev.ID
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
		armAutoPick(cfg, st)
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

// armAutoPick — на своём сборе с голосованием к дедлайну выбрать место самим, если
// инициатор не выбрал раньше. Вызывать под callsMu; повторный вызов ничего не делает.
func armAutoPick(cfg Config, st *callState) {
	if st.autoArmed || len(st.Options) == 0 {
		return
	}
	st.autoArmed = true
	id := st.ID
	time.AfterFunc(time.Until(st.Deadline), func() { autoPick(cfg, id) })
}

// autoPick — время вышло, а места нет: больше всего голосов, при равенстве — жребий.
// Инициатор потом может передумать кнопкой 🏆.
func autoPick(cfg Config, id string) {
	callsMu.Lock()
	st, ok := calls[id]
	if !ok || st.Winner != "" || len(st.Options) == 0 {
		callsMu.Unlock()
		return
	}
	place, why := chooseWinner(pollOptions(st))
	callsMu.Unlock()
	pickWith(cfg, id, place, why)
	refresh(id)
}

// chooseWinner — место с наибольшим числом голосов; при ничьей — случайное из лидеров.
func chooseWinner(opts []uiOption) (place, why string) {
	best := 0
	for _, o := range opts {
		best = max(best, len(o.Voters))
	}
	var top []string
	for _, o := range opts {
		if len(o.Voters) == best {
			top = append(top, o.Name)
		}
	}
	if len(top) == 0 {
		return "", ""
	}
	place = top[rand.IntN(len(top))]
	switch {
	case best == 0:
		why = "никто не голосовал — выбрал жребий"
	case len(top) == 1:
		why = "больше всего голосов"
	default:
		why = fmt.Sprintf("ничья между %d местами — выбрал жребий", len(top))
	}
	return place, why
}

// startCall — позвать всех из трея. Инициатор сразу считается принявшим, как в Доте.
func startCall(cfg Config, a Activity) error {
	if a.votes() {
		a.options = pickOptions(cfg.LunchPlaces, voteOptions)
	}
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
		armAutoPick(cfg, st)
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

// Локальная проверка окна — никому не отправляется. poll — с голосованием «куда идём»,
// как будто позвал ты сам (можно выбрать победителя).
func testCall(cfg Config, poll bool) {
	id := "local-" + time.Now().Format("150405.000")
	now := time.Now()
	st := &callState{
		ID: id, Title: "Проверка", Message: "Так будет выглядеть сбор", From: "Вася",
		Start: now, Deadline: now.Add(time.Duration(cfg.ReadyTimeout) * time.Second),
		Local: true, Responses: map[string]response{},
	}
	if poll && len(cfg.LunchPlaces) > 0 {
		st.Title, st.Message, st.From = "Обед", "🍲 Сбор на обед! (проверка)", cleanName(cfg.Name)
		st.Options = pickOptions(cfg.LunchPlaces, voteOptions)
		addResponse(st, response{Call: id, Client: clientID, Name: cfg.Name, Answer: ansAccept})
		addResponse(st, response{Call: id, Client: "demo1", Name: "Вася", Answer: ansAccept, Vote: st.Options[0]})
		addResponse(st, response{Call: id, Client: "demo4", Name: "Маша", Answer: ansAccept, Vote: st.Options[0]})
		addResponse(st, response{Call: id, Client: "demo5", Name: "Дима", Answer: ansAccept, Vote: "Шаверма у метро"})
		addResponse(st, response{Call: id, Client: "demo2", Name: "Петя", Answer: ansDecline, Reason: "Уже поел"})
		addResponse(st, response{Call: id, Client: "demo3", Name: "Коля", Answer: ansSeen})
		armAutoPick(cfg, st)
	} else {
		addResponse(st, response{Call: id, Client: "demo1", Name: "Вася", Answer: ansAccept})
		addResponse(st, response{Call: id, Client: "demo2", Name: "Петя", Answer: ansDecline, Reason: "На созвоне до 15:00"})
		addResponse(st, response{Call: id, Client: "demo3", Name: "Коля", Answer: ansSeen})
		addResponse(st, response{Call: id, Client: clientID, Name: cfg.Name, Answer: ansSeen})
	}
	callsMu.Lock()
	calls[id] = st
	callsMu.Unlock()
	showReady(cfg, id)
}

func addResponse(st *callState, r response) {
	if r.Answer == ansPick { // не ответ человека, а итог голосования — в список не попадает
		if len(st.Options) > 0 && r.Vote != "" {
			st.Winner, st.WinnerBy, st.WinnerWhy = r.Vote, r.Name, r.Reason
		}
		return
	}
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

func onResponse(cfg Config, r response) {
	callsMu.Lock()
	st, ok := calls[r.Call]
	reopen := false
	if ok {
		old := st.Winner
		addResponse(st, r)
		// Инициатор выбрал место: тем, кто не отказался, показываем итог,
		// даже если окно уже закрылось.
		reopen = r.Answer == ansPick && st.Winner != old && r.Client != clientID &&
			st.Responses[clientID].Answer != ansDecline
	} else if len(pending) < 100 {
		pending[r.Call] = append(pending[r.Call], r)
	}
	callsMu.Unlock()
	if !ok {
		return
	}
	if reopen && !windowOpen() {
		showReady(cfg, r.Call)
		return
	}
	refresh(r.Call)
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
	s.Options = pollOptions(st)
	s.Winner, s.WinnerBy, s.WinnerWhy = st.Winner, st.WinnerBy, st.WinnerWhy
	_, mine := myCalls[id]
	s.CanPick = len(st.Options) > 0 && (mine || st.Local)
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

// pollOptions — строки голосования: сначала места из сбора, потом вписанные в «Другое».
func pollOptions(st *callState) []uiOption {
	opts := []uiOption{}
	if len(st.Options) == 0 {
		return opts
	}
	find := func(name string) int {
		return slices.IndexFunc(opts, func(o uiOption) bool { return strings.EqualFold(o.Name, name) })
	}
	add := func(name string) int {
		if i := find(name); i >= 0 {
			return i
		}
		opts = append(opts, uiOption{Name: name, Voters: []string{}, Custom: len(opts) >= len(st.Options)})
		return len(opts) - 1
	}
	for _, o := range st.Options {
		add(o)
	}
	for _, c := range st.Order {
		r := st.Responses[c]
		if r.Answer != ansAccept || r.Vote == "" {
			continue
		}
		i := add(r.Vote)
		opts[i].Voters = append(opts[i].Voters, r.Name)
		opts[i].Mine = opts[i].Mine || c == clientID
	}
	if st.Winner != "" {
		add(st.Winner) // выбрали место, за которое уже никто не голосует
	}
	return opts
}

// answer — человек нажал кнопку; reason учитывается только для отказа,
// vote (место) — только для «Принять» в сборе с голосованием.
func answer(cfg Config, id, ans, reason, vote string) uiState {
	if ans != ansAccept && ans != ansDecline {
		return snapshot(id)
	}
	reason = cleanReason(reason)
	if ans != ansDecline {
		reason = ""
	}
	vote = cleanVote(vote)
	callsMu.Lock()
	st, ok := calls[id]
	var r, prev response
	send := false
	if ok && time.Now().Before(st.Deadline) {
		polling := len(st.Options) > 0 && st.Winner == ""
		if !polling || ans != ansAccept {
			vote = ""
		}
		prev = st.Responses[clientID]
		// Пока голосование идёт, голос можно поменять — это просто новый accept.
		revote := polling && ans == ansAccept && prev.Answer == ansAccept && vote != prev.Vote
		if !answered(st, clientID) || revote {
			r = response{Call: id, Client: clientID, Name: cfg.Name, Answer: ans, Reason: reason, Vote: vote}
			addResponse(st, r)
			send = !st.Local
		}
	}
	callsMu.Unlock()

	if send {
		go func() {
			if err := sendResponse(cfg, r); err != nil {
				revertAnswer(id, r, prev)
				refresh(id)
				messageBox("Office Meetings", "Ответ не ушёл: "+err.Error()+"\n\nНажми ещё раз.", mbOK|mbIconError)
			}
		}()
	}
	return snapshot(id)
}

// revertAnswer — ответ так и не ушёл: возвращаем прежний (или снова «молчим»), чтобы
// кнопки в окне ожили. Если ответ на самом деле дошёл (оборвалось уже после),
// его эхо из ntfy вернёт всё как было.
func revertAnswer(id string, failed, prev response) {
	callsMu.Lock()
	defer callsMu.Unlock()
	st, ok := calls[id]
	if !ok || st.Responses[failed.Client] != failed {
		return
	}
	if prev.Answer == ansAccept || prev.Answer == ansDecline {
		st.Responses[failed.Client] = prev
		return
	}
	failed.Answer, failed.Reason, failed.Vote = ansSeen, "", ""
	st.Responses[failed.Client] = failed
}

// pick — инициатор выбрал, куда идём. Повторный выбор меняет решение.
func pick(cfg Config, id, place string) uiState { return pickWith(cfg, id, place, "") }

// pickWith — выбор места; why — как выбрала программа (пусто — выбрал человек).
func pickWith(cfg Config, id, place, why string) uiState {
	place = cleanVote(place)
	callsMu.Lock()
	st, ok := calls[id]
	_, mine := myCalls[id]
	var r response
	var oldW, oldBy, oldWhy string
	send := false
	if ok && place != "" && len(st.Options) > 0 && (mine || st.Local) && place != st.Winner {
		oldW, oldBy, oldWhy = st.Winner, st.WinnerBy, st.WinnerWhy
		r = response{Call: id, Client: clientID, Name: cfg.Name, Answer: ansPick, Vote: place, Reason: why}
		addResponse(st, r)
		send = !st.Local
	}
	callsMu.Unlock()

	if send {
		go func() {
			if err := sendResponse(cfg, r); err != nil {
				callsMu.Lock()
				if st.Winner == place {
					st.Winner, st.WinnerBy, st.WinnerWhy = oldW, oldBy, oldWhy
				}
				callsMu.Unlock()
				refresh(id)
				messageBox("Office Meetings", "Выбор не ушёл: "+err.Error()+"\n\nВыбери место ещё раз.", mbOK|mbIconError)
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

func windowOpen() bool {
	winMu.Lock()
	defer winMu.Unlock()
	return curWin != nil
}

// reopenLast — открыть окно последнего сбора (пункт «Последний: …» в трее).
func reopenLast(cfg Config) {
	callsMu.Lock()
	id := lastCall
	_, ok := calls[id]
	callsMu.Unlock()
	if ok {
		openWindow(cfg, id)
	}
}

func showReady(cfg Config, id string) {
	beep()
	openWindow(cfg, id)
}

func openWindow(cfg Config, id string) {
	winMu.Lock()
	if curWin != nil { // новый сбор вытесняет старое окно
		// Только Destroy (WM_CLOSE → DestroyWindow → WM_DESTROY → выход из Run).
		// Terminate лишь глушит цикл сообщений, а окно остаётся висеть «Не отвечает».
		curWin.Destroy()
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
	height := 580
	if len(st.Options) > 0 {
		height = 740 // голосование занимает место
	}
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  webviewDataPath(),
		WindowOptions: webview2.WindowOptions{
			Title:  "Сбор: " + st.Title,
			Width:  440,
			Height: uint(height),
			Center: true,
		},
	})
	if w == nil { // нет WebView2 — показываем простое окно
		notify(st.Title, strings.TrimSpace(st.Message))
		return
	}
	defer w.Destroy()
	w.SetSize(440, height, webview2.HintFixed)

	_ = w.Bind("getState", func() uiState { return snapshot(id) })
	_ = w.Bind("answer", func(a, reason, vote string) uiState { return answer(cfg, id, a, reason, vote) })
	_ = w.Bind("pick", func(place string) uiState { return pick(cfg, id, place) })
	_ = w.Bind("closeWindow", func() { w.Destroy() }) // см. showReady: не Terminate
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
