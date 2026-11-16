package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ntfyEvent struct {
	ID      string `json:"id"`
	Time    int64  `json:"time"`
	Event   string `json:"event"`
	Topic   string `json:"topic"`
	Title   string `json:"title"`
	Message string `json:"message"`
}

// response — ответ «принял/отклонил» на конкретный сбор.
type response struct {
	Call   string `json:"call"`   // id сообщения-сбора в ntfy
	Client string `json:"client"` // id копии программы (чтобы не путать тёзок)
	Name   string `json:"name"`
	Answer string `json:"answer"` // accept | decline | seen
}

// Варианты ответа. seen — «сбор дошёл, окно показано», шлётся автоматически
// при получении сбора; accept/decline — то, что человек нажал.
const (
	ansAccept  = "accept"
	ansDecline = "decline"
	ansSeen    = "seen"
)

func validAnswer(a string) bool { return a == ansAccept || a == ansDecline || a == ansSeen }

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
				onResponse(r)
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

func publishRaw(cfg Config, topic, title, message string, priority int) error {
	if priority == 0 {
		priority = 4
	}
	body, _ := json.Marshal(map[string]any{
		"topic":    topic,
		"title":    title,
		"message":  message,
		"priority": priority,
	})
	req, err := http.NewRequest(http.MethodPost, cfg.Server+"/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := newClient(cfg, 10*time.Second).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func publish(cfg Config, a Activity) error {
	return publishRaw(cfg, cfg.Topic, a.Title, a.Message, a.Priority)
}

func sendResponse(cfg Config, r response) error {
	b, _ := json.Marshal(r)
	return publishRaw(cfg, cfg.RespTopic(), "ready", string(b), 1)
}
