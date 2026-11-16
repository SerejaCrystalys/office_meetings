package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func newPollCall(id string, opts ...string) *callState {
	st := newTestCall(id)
	callsMu.Lock()
	st.Options = opts
	callsMu.Unlock()
	return st
}

func optionByName(s uiState, name string) *uiOption {
	for i := range s.Options {
		if s.Options[i].Name == name {
			return &s.Options[i]
		}
	}
	return nil
}

func TestVoteTags(t *testing.T) {
	if got := voteTag("  Пхали, Хинкали \n(Литейный 46) "); got != "vote:Пхали Хинкали (Литейный 46)" {
		t.Errorf("тег места: %q", got)
	}
	ev := ntfyEvent{Tags: []string{"from:Я", "vote:А", "vote:Б", "vote:", "vote:В", "vote:Г", "vote:Д", "vote:Е"}}
	got := ev.options()
	if len(got) != voteOptions || got[0] != "А" || got[4] != "Д" {
		t.Errorf("ждали первые %d мест по порядку, получили %v", voteOptions, got)
	}
	if (ntfyEvent{Tags: []string{"from:Я"}}).options() != nil {
		t.Error("сбор без голосования — вариантов быть не должно")
	}
}

func TestPickOptions(t *testing.T) {
	places := []string{"1", "2", "3", "4", "5", "6", "7"}
	got := pickOptions(places, voteOptions)
	if len(got) != voteOptions {
		t.Fatalf("ждали %d, получили %v", voteOptions, got)
	}
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p] {
			t.Fatalf("повтор места: %v", got)
		}
		seen[p] = true
	}
	if len(pickOptions([]string{"1", "2"}, voteOptions)) != 2 {
		t.Error("мест меньше пяти — берём все")
	}
	if places[0] != "1" || places[6] != "7" {
		t.Error("исходный список не должен перемешиваться")
	}
}

// Голос = «Принять» с местом; пока нет победителя, голос можно поменять.
func TestVoteAndRevote(t *testing.T) {
	clientID = "me"
	cfg := Config{Name: "Я"}
	st := newPollCall("t-vote", "Марчеллис", "Пхали")
	addResponse(st, response{Call: st.ID, Client: "v", Name: "Вася", Answer: ansAccept, Vote: "марчеллис"})

	s := answer(cfg, st.ID, ansAccept, "", "Пхали")
	if s.Mine != ansAccept || !optionByName(s, "Пхали").Mine {
		t.Fatalf("голос за Пхали должен засчитаться как «Принять»: %+v", s)
	}
	if o := optionByName(s, "Марчеллис"); len(o.Voters) != 1 || o.Voters[0] != "Вася" {
		t.Fatalf("голос другим регистром должен попасть в то же место: %+v", s.Options)
	}

	s = answer(cfg, st.ID, ansAccept, "", "Шаверма у метро")
	if len(optionByName(s, "Пхали").Voters) != 0 {
		t.Fatal("после смены голоса старое место должно опустеть")
	}
	if o := optionByName(s, "Шаверма у метро"); o == nil || !o.Custom || !o.Mine {
		t.Fatalf("своё место должно появиться отдельной строкой: %+v", s.Options)
	}
	if len(s.Options) != 3 {
		t.Fatalf("ждали 3 строки, получили %+v", s.Options)
	}

	// После отказа голосовать нельзя, а отказ после «Принять» — не принимается.
	if s = answer(cfg, st.ID, ansDecline, "", ""); s.Mine != ansAccept {
		t.Fatalf("уже принял — отказ не должен приниматься, Mine=%q", s.Mine)
	}
}

func TestVoteIgnoredWithoutPoll(t *testing.T) {
	clientID = "me"
	st := newTestCall("t-novote")
	answer(Config{Name: "Я"}, st.ID, ansAccept, "", "Куда-то")
	if st.Responses["me"].Vote != "" {
		t.Fatal("в сборе без голосования место не запоминаем")
	}
}

func TestPick(t *testing.T) {
	clientID = "me"
	cfg := Config{Name: "Я"}
	st := newPollCall("t-pick", "Марчеллис", "Пхали")
	callsMu.Lock()
	st.Local = false // чужой сетевой сбор: выбирать нельзя
	callsMu.Unlock()
	if s := pick(cfg, st.ID, "Пхали"); s.Winner != "" || s.CanPick {
		t.Fatalf("чужой сбор — выбирать нельзя: %+v", s)
	}
	callsMu.Lock()
	st.Local = true
	callsMu.Unlock()

	answer(cfg, st.ID, ansAccept, "", "Марчеллис")
	s := pick(cfg, st.ID, "Пхали")
	if s.Winner != "Пхали" || s.WinnerBy != "Я" || !s.CanPick {
		t.Fatalf("ждали победителя Пхали: %+v", s)
	}
	if s.Mine != ansAccept || st.Responses["me"].Answer != ansAccept {
		t.Fatal("выбор победителя не должен перетирать свой ответ")
	}
	if s = answer(cfg, st.ID, ansAccept, "", "Пхали"); !optionByName(s, "Марчеллис").Mine {
		t.Fatal("после выбора победителя голос не меняется")
	}
}

// Выбор, пришедший раньше самого сбора, применяется, когда сбор дойдёт.
func TestPickFromNetwork(t *testing.T) {
	st := newPollCall("t-pick-net", "Марчеллис")
	addResponse(st, response{Call: st.ID, Client: "boss", Name: "Босс", Answer: ansPick, Vote: "Своё место"})
	s := snapshot(st.ID)
	if s.Winner != "Своё место" || s.WinnerBy != "Босс" {
		t.Fatalf("ждали победителя от Босса: %+v", s)
	}
	if o := optionByName(s, "Своё место"); o == nil || !o.Custom {
		t.Fatalf("выбранное место должно быть в списке: %+v", s.Options)
	}
	if _, ok := st.Responses["boss"]; ok {
		t.Fatal("pick не должен попадать в список ответивших")
	}
	plain := newTestCall("t-pick-plain")
	addResponse(plain, response{Call: plain.ID, Client: "boss", Answer: ansPick, Vote: "X"})
	if plain.Winner != "" {
		t.Fatal("в сборе без голосования победителя нет")
	}
}

// Смена голоса не ушла — возвращается прежний голос, а не «молчит».
func TestRevertRevote(t *testing.T) {
	clientID = "me"
	st := newPollCall("t-revert-vote", "А", "Б")
	prev := response{Call: st.ID, Client: "me", Name: "Я", Answer: ansAccept, Vote: "А"}
	addResponse(st, prev)
	answer(Config{Name: "Я"}, st.ID, ansAccept, "", "Б")
	revertAnswer(st.ID, response{Call: st.ID, Client: "me", Name: "Я", Answer: ansAccept, Vote: "Б"}, prev)
	if st.Responses["me"] != prev {
		t.Fatalf("ждали прежний голос, получили %+v", st.Responses["me"])
	}
}

// Сбор с голосованием уходит с тегами vote:… — пятью случайными местами.
func TestStartCallSendsOptions(t *testing.T) {
	clientID = "me"
	var tags []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m struct {
			Tags []string `json:"tags"`
		}
		_ = json.Unmarshal(b, &m)
		if tags == nil {
			tags = m.Tags
		}
		_, _ = w.Write([]byte(`{"id":"call-poll","event":"message"}`))
	}))
	t.Cleanup(srv.Close)
	cfg := Config{Server: srv.URL, Topic: "t", Name: "Я", LunchPlaces: []string{"1", "2", "3", "4", "5", "6"}}
	if err := startCall(cfg, Activity{Title: "Обед", Message: "Сбор!"}); err != nil {
		t.Fatal(err)
	}
	ev := ntfyEvent{Tags: tags}
	if len(ev.options()) != voteOptions || ev.initiator() != "Я" {
		t.Fatalf("ждали from:Я и %d мест, получили %v", voteOptions, tags)
	}

	tags = nil
	no := false
	if err := startCall(cfg, Activity{Title: "Обед", Message: "Сбор!", Vote: &no}); err != nil {
		t.Fatal(err)
	}
	if len((ntfyEvent{Tags: tags}).options()) != 0 {
		t.Fatalf("vote:false — без мест, получили %v", tags)
	}
}

func TestLunchPlacesConfig(t *testing.T) {
	p := t.TempDir() + "/config.json"
	load := func(body string) Config {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfigFrom(p)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	if cfg := load(`{"topic":"t"}`); len(cfg.LunchPlaces) != len(defaultLunchPlaces) {
		t.Errorf("нет ключа — ждали список у офиса, получили %v", cfg.LunchPlaces)
	}
	if cfg := load(`{"topic":"t","lunch_places":[]}`); len(cfg.LunchPlaces) != 0 {
		t.Errorf("[] — без голосования, получили %v", cfg.LunchPlaces)
	}
	cfg := load(`{"topic":"t","lunch_places":[" Пхали,  Хинкали ","","пхали хинкали","Марчеллис"]}`)
	if len(cfg.LunchPlaces) != 2 || cfg.LunchPlaces[0] != "Пхали Хинкали" {
		t.Errorf("ждали чистку и без дублей, получили %v", cfg.LunchPlaces)
	}
	cfg = load(`{"topic":"t","activities":[{"title":"Обед"},{"title":"Кофе"},{"title":"Кофе2","vote":true},{"title":"Обед","vote":false}]}`)
	want := []bool{true, false, true, false}
	for i, a := range cfg.Activities {
		if a.votes() != want[i] {
			t.Errorf("активность %d (%s): votes=%v", i, a.Title, a.votes())
		}
	}
}

func TestChooseWinner(t *testing.T) {
	opt := func(name string, votes int) uiOption {
		o := uiOption{Name: name, Voters: []string{}}
		for i := 0; i < votes; i++ {
			o.Voters = append(o.Voters, "x")
		}
		return o
	}
	if p, why := chooseWinner([]uiOption{opt("А", 1), opt("Б", 3), opt("В", 2)}, seededRand("x")); p != "Б" || why != "больше всего голосов" {
		t.Errorf("ждали Б по голосам, получили %q (%s)", p, why)
	}
	// Ничья: оба лидера должны выпадать, остальные — никогда.
	got := map[string]bool{}
	for i := 0; i < 200; i++ {
		p, why := chooseWinner([]uiOption{opt("А", 2), opt("Б", 0), opt("В", 2)}, seededRand(fmt.Sprint(i)))
		if p == "Б" || why == "" {
			t.Fatalf("при ничьей выбираем только из лидеров, получили %q (%s)", p, why)
		}
		got[p] = true
	}
	if !got["А"] || !got["В"] {
		t.Errorf("жребий должен выбирать обоих лидеров, выпали %v", got)
	}
	if p, _ := chooseWinner([]uiOption{opt("А", 0), opt("Б", 0)}, seededRand("x")); p == "" {
		t.Error("никто не голосовал — всё равно выбираем место")
	}
}

// Время вышло, инициатор не выбрал — программа выбирает сама; ручной выбор не перетирает.
func TestAutoPick(t *testing.T) {
	clientID = "me"
	cfg := Config{Name: "Я"}
	st := newPollCall("t-auto", "А", "Б")
	addResponse(st, response{Call: st.ID, Client: "v", Name: "Вася", Answer: ansAccept, Vote: "Б"})
	autoPick(cfg, st.ID)
	s := snapshot(st.ID)
	if s.Winner != "Б" || s.WinnerWhy == "" {
		t.Fatalf("ждали автовыбор Б с причиной: %+v", s)
	}

	st2 := newPollCall("t-auto-manual", "А", "Б")
	addResponse(st2, response{Call: st2.ID, Client: "v", Name: "Вася", Answer: ansAccept, Vote: "Б"})
	pick(cfg, st2.ID, "А")
	autoPick(cfg, st2.ID)
	if s = snapshot(st2.ID); s.Winner != "А" || s.WinnerWhy != "" {
		t.Fatalf("ручной выбор не должен перетираться автовыбором: %+v", s)
	}
}

// Таймер автовыбора срабатывает к дедлайну.
func TestArmAutoPick(t *testing.T) {
	clientID = "me"
	st := newPollCall("t-arm", "А")
	callsMu.Lock()
	st.Deadline = time.Now().Add(50 * time.Millisecond)
	armAutoPick(Config{Name: "Я"}, st)
	armAutoPick(Config{Name: "Я"}, st) // повторно — без второго таймера
	callsMu.Unlock()
	time.Sleep(300 * time.Millisecond)
	if s := snapshot(st.ID); s.Winner != "А" {
		t.Fatalf("к дедлайну место должно выбраться само: %+v", s)
	}
}

// Сбор «Обед» с кнопки: у всех копий одни и те же места (по id сбора), без инициатора.
func TestButtonLunchCall(t *testing.T) {
	places := []string{"1", "2", "3", "4", "5", "6", "7", "8"}
	cfg := Config{ReadyTimeout: 60, LunchPlaces: places, Activities: defaultActivities}
	ev := ntfyEvent{ID: "btn-1", Time: time.Now().Unix(), Title: "Обед", Message: "Сбор на обед!"}
	a, b := callFromEvent(cfg, ev), callFromEvent(cfg, ev)
	if !a.Shared || len(a.Options) != voteOptions || fmt.Sprint(a.Options) != fmt.Sprint(b.Options) {
		t.Fatalf("ждали одинаковые %d мест у всех копий: %v / %v", voteOptions, a.Options, b.Options)
	}
	ev2 := ev
	ev2.ID = "btn-2"
	diff := false
	for i := 0; i < 5 && !diff; i++ { // у разных сборов места разные (почти всегда)
		ev2.ID = fmt.Sprint("btn-x", i)
		diff = fmt.Sprint(callFromEvent(cfg, ev2).Options) != fmt.Sprint(a.Options)
	}
	if !diff {
		t.Error("у разных сборов места должны меняться")
	}
	if c := callFromEvent(cfg, ntfyEvent{ID: "btn-3", Title: "Курилка"}); len(c.Options) != 0 {
		t.Errorf("курилка с кнопки — без голосования: %v", c.Options)
	}
	if c := callFromEvent(cfg, ntfyEvent{ID: "tray-1", Title: "Обед", Tags: []string{"from:Вася"}}); c.Shared || len(c.Options) != 0 {
		t.Errorf("сбор из трея без мест (старая версия) — сами места не придумываем: %v", c.Options)
	}
}

// К дедлайну сбора с кнопки каждая копия считает итог сама и одинаково (жребий по id).
func TestButtonAutoPick(t *testing.T) {
	clientID = "me"
	results := map[string]bool{}
	for i := 0; i < 2; i++ { // две «копии» с одинаковыми голосами
		id := "btn-auto"
		st := newPollCall(fmt.Sprint(id, i), "А", "Б", "В")
		callsMu.Lock()
		st.ID, st.Local, st.Shared = id, false, true
		delete(calls, fmt.Sprint(id, i))
		calls[id] = st
		callsMu.Unlock()
		addResponse(st, response{Call: id, Client: "me", Name: "Я", Answer: ansDecline}) // окно не открываем
		addResponse(st, response{Call: id, Client: "v", Name: "Вася", Answer: ansAccept, Vote: "А"})
		addResponse(st, response{Call: id, Client: "m", Name: "Маша", Answer: ansAccept, Vote: "В"})
		autoPick(Config{Name: "Я"}, id)
		s := snapshot(id)
		if s.Winner != "А" && s.Winner != "В" || s.WinnerWhy == "" || s.CanPick {
			t.Fatalf("ждали жребий между А и В без кнопки 🏆: %+v", s)
		}
		results[s.Winner] = true
	}
	if len(results) != 1 {
		t.Fatalf("у копий с одинаковыми голосами итог должен совпасть: %v", results)
	}
}

func TestFunWAV(t *testing.T) {
	b := funWAV()
	if string(b[:4]) != "RIFF" || string(b[8:16]) != "WAVEfmt " || string(b[36:40]) != "data" {
		t.Fatal("заголовок WAV неправильный")
	}
	if n := len(b) - 44; n != int(22050*1.8)*2 { // 1,8 с × 22 050 Гц × 2 байта
		t.Fatalf("ждали 1,8 с звука, байт данных %d", n)
	}
}

func TestSoundConfig(t *testing.T) {
	p := t.TempDir() + "/config.json"
	for body, want := range map[string]string{
		`{"topic":"t"}`:                soundFun,
		`{"topic":"t","sound":"OFF"}`:  soundOff,
		`{"topic":"t","sound":"beep"}`: soundBeep,
		`{"topic":"t","sound":"бум"}`:  soundFun,
	} {
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfigFrom(p)
		if err != nil || cfg.Sound != want {
			t.Errorf("%s: ждали %q, получили %q (%v)", body, want, cfg.Sound, err)
		}
	}
}
