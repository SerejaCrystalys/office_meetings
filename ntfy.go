package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ntfyEvent struct {
	ID      string   `json:"id"`
	Time    int64    `json:"time"`
	Event   string   `json:"event"`
	Topic   string   `json:"topic"`
	Title   string   `json:"title"`
	Message string   `json:"message"`
	Tags    []string `json:"tags"`
}

// Инициатор сбора едет в теге «from:<имя>» — его видят и программы, и телефоны с ntfy.
// У сборов с кнопки тега нет.
const fromTagPrefix = "from:"

func fromTag(name string) string {
	return fromTagPrefix + strings.ReplaceAll(cleanName(name), ",", " ")
}

// initiator — имя из тега from:, "" если сбор с кнопки (или от старой версии программы).
func (ev ntfyEvent) initiator() string {
	for _, t := range ev.Tags {
		if strings.HasPrefix(t, fromTagPrefix) {
			return cleanName(strings.TrimPrefix(t, fromTagPrefix))
		}
	}
	return ""
}

// Варианты голосования «куда идём» едут тегами «vote:<место>» в том же сообщении-сборе.
const voteTagPrefix = "vote:"

func voteTag(place string) string { return voteTagPrefix + cleanVote(place) }

// options — варианты голосования из тегов сбора (в том порядке, как их отправили).
func (ev ntfyEvent) options() []string {
	var out []string
	for _, t := range ev.Tags {
		if strings.HasPrefix(t, voteTagPrefix) {
			if o := cleanVote(strings.TrimPrefix(t, voteTagPrefix)); o != "" && len(out) < voteOptions {
				out = append(out, o)
			}
		}
	}
	return out
}

const maxVoteLen = 60 // символов

// cleanVote — название места в одну строку, без запятых (ntfy режет по ним теги).
func cleanVote(s string) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, ",", " ")), " ")
	if r := []rune(s); len(r) > maxVoteLen {
		s = string(r[:maxVoteLen])
	}
	return s
}

const maxNameLen = 32

// cleanName — имя в одну строку, не длиннее maxNameLen символов.
func cleanName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxNameLen {
		s = string(r[:maxNameLen])
	}
	return s
}

// response — ответ «принял/отклонил» на конкретный сбор.
type response struct {
	Call   string `json:"call"`   // id сообщения-сбора в ntfy
	Client string `json:"client"` // id копии программы (чтобы не путать тёзок)
	Name   string `json:"name"`
	Answer string `json:"answer"`           // accept | decline | seen | pick
	Reason string `json:"reason,omitempty"` // почему отклонил (только для decline, необязательно)
	Vote   string `json:"vote,omitempty"`   // accept: за какое место голос; pick: выбранное место
}

const maxReasonLen = 80 // символов

// cleanReason — одна строка без лишних пробелов, не длиннее maxReasonLen.
func cleanReason(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxReasonLen {
		s = string(r[:maxReasonLen])
	}
	return s
}

// Варианты ответа. seen — «сбор дошёл, окно показано», шлётся автоматически
// при получении сбора; accept/decline — то, что человек нажал.
const (
	ansAccept  = "accept"
	ansDecline = "decline"
	ansSeen    = "seen"
	ansPick    = "pick" // инициатор выбрал, куда идём (место в Vote); старые версии такое игнорируют
)

func validAnswer(a string) bool {
	return a == ansAccept || a == ansDecline || a == ansSeen || a == ansPick
}

func subscribe(cfg Config) {
	client := newClient(cfg, 0) // поток бесконечный, общий таймаут не ставим
	var lastID string
	backoff := 2 * time.Second
	host := strings.TrimPrefix(strings.TrimPrefix(cfg.Server, "https://"), "http://")

	for {
		err := streamOnce(cfg, client, &lastID, func() {
			backoff = 2 * time.Second
			setOnline(true, host)
		})
		msg := "соединение закрыто"
		if err != nil {
			msg = err.Error()
		}
		if r := []rune(msg); len(r) > 60 {
			msg = string(r[:60]) + "…"
		}
		setOnline(false, msg)
		time.Sleep(backoff)
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func streamOnce(cfg Config, client *http.Client, lastID *string, onConnected func()) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	u := fmt.Sprintf("%s/%s,%s/json", cfg.Server, url.PathEscape(cfg.Topic), url.PathEscape(cfg.RespTopic()))
	if *lastID != "" {
		u += "?since=" + url.QueryEscape(*lastID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	onConnected()

	watchdog := time.AfterFunc(silenceLimit, cancel)
	defer watchdog.Stop()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		watchdog.Reset(silenceLimit)
		var ev ntfyEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Event != "message" {
			continue
		}
		*lastID = ev.ID
		sent := time.Unix(ev.Time, 0)
		if time.Since(sent) > staleAfter {
			continue
		}

		if ev.Topic == cfg.RespTopic() {
			var r response
			if json.Unmarshal([]byte(ev.Message), &r) == nil && r.Call != "" && r.Client != "" &&
				validAnswer(r.Answer) {
				r.Reason = cleanReason(r.Reason)
				if r.Answer != ansDecline && r.Answer != ansPick { // у pick — как выбрала программа
					r.Reason = ""
				}
				r.Vote = cleanVote(r.Vote)
				if r.Answer != ansAccept && r.Answer != ansPick {
					r.Vote = ""
				}
				if r.Answer != ansPick || r.Vote != "" {
					onResponse(cfg, r)
				}
			}
			continue
		}
		onCall(cfg, ev)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("сервер закрыл соединение")
}

// publishRaw отправляет сообщение и возвращает его id в ntfy.
func publishRaw(cfg Config, topic, title, message string, priority int, tags []string) (string, error) {
	if priority == 0 {
		priority = 4
	}
	msg := map[string]any{
		"topic":    topic,
		"title":    title,
		"message":  message,
		"priority": priority,
	}
	if len(tags) > 0 {
		msg["tags"] = tags
	}
	body, _ := json.Marshal(msg)
	req, err := http.NewRequest(http.MethodPost, cfg.Server+"/", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := newClient(cfg, 10*time.Second).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", statusError(resp.StatusCode)
	}
	var ev ntfyEvent
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&ev)
	return ev.ID, nil
}

// statusError — сервер ответил, но не 200.
type statusError int

func (e statusError) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

// Стоит ли повторить отправку: обрыв/таймаут сети или сбой на стороне сервера (5xx).
// 4xx (неверный топик, лимит 429 и т.п.) повтором не лечатся.
func retryable(err error) bool {
	var se statusError
	if errors.As(err, &se) {
		return int(se) >= 500
	}
	return err != nil
}

// Паузы перед повторами отправки ответа (первая попытка — сразу).
var retryPauses = []time.Duration{0, 700 * time.Millisecond, 2 * time.Second}

// publish отправляет сбор от имени cfg.Name и возвращает id сбора в ntfy.
func publish(cfg Config, a Activity) (string, error) {
	tags := []string{fromTag(cfg.Name)}
	for _, o := range a.options {
		tags = append(tags, voteTag(o))
	}
	return publishRaw(cfg, cfg.Topic, a.Title, a.Message, a.Priority, tags)
}

// sendResponse повторяет отправку при обрыве: дубль ответа безвреден — от одной копии
// программы учитывается последний. Сборы (publish) намеренно не повторяем: дубль
// открыл бы всем окно второй раз.
func sendResponse(cfg Config, r response) error {
	b, _ := json.Marshal(r)
	var err error
	for _, pause := range retryPauses {
		time.Sleep(pause)
		if _, err = publishRaw(cfg, cfg.RespTopic(), "ready", string(b), 1, nil); !retryable(err) {
			return err
		}
	}
	return err
}
