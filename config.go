package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Activity — то, на что можно позвать из трея (курилка, обед, кофе, созвон…).
type Activity struct {
	Menu     string `json:"menu,omitempty"`     // текст пункта в трее (по умолчанию «Позвать: <title>»)
	Title    string `json:"title"`              // заголовок уведомления
	Message  string `json:"message"`            // текст уведомления
	Priority int    `json:"priority,omitempty"` // 1–5, по умолчанию 4
}

var defaultActivities = []Activity{
	{Menu: "🚬 Позвать на курилку", Title: "Курилка", Message: "🚬 Сбор на курилку!"},
	{Menu: "🍲 Позвать на обед", Title: "Обед", Message: "🍲 Сбор на обед!"},
}

type Config struct {
	Name         string     `json:"name"` // как тебя видят остальные в окне сбора
	Server       string     `json:"server"`
	Topic        string     `json:"topic"`
	Token        string     `json:"token"`
	Proxy        string     `json:"proxy"`
	UpdateRepo   string     `json:"update_repo"`
	ReadyTimeout int        `json:"ready_timeout"` // секунд на ответ «принять/отклонить»
	Activities   []Activity `json:"activities"`
}

// Топик для ответов «принял/отклонил» — отдельный, чтобы не шуметь в телефонах.
func (c Config) RespTopic() string { return c.Topic + "-r" }

func configPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}

func defaultName() string {
	if n := strings.TrimSpace(os.Getenv("USERNAME")); n != "" {
		return n
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "Аноним"
}

func loadConfig() (Config, error) {
	path := configPath()
	cfg := Config{
		Name:         defaultName(),
		Server:       defaultServer,
		Topic:        defaultTopic,
		UpdateRepo:   defaultUpdateRepo,
		ReadyTimeout: 60,
		Activities:   defaultActivities,
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		out, _ := json.MarshalIndent(cfg, "", "  ")
		_ = os.WriteFile(path, out, 0644)
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}

	cfg.Activities = nil
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("ошибка в config.json: %w", err)
	}

	cfg.Name = strings.TrimSpace(cfg.Name)
	if cfg.Name == "" {
		cfg.Name = defaultName()
	}
	if r := []rune(cfg.Name); len(r) > 32 {
		cfg.Name = string(r[:32])
	}
	cfg.Server = strings.TrimRight(strings.TrimSpace(cfg.Server), "/")
	if cfg.Server == "" {
		cfg.Server = defaultServer
	}
	cfg.Topic = strings.TrimSpace(cfg.Topic)
	if cfg.Topic == "" {
		cfg.Topic = defaultTopic
	}
	if cfg.Topic == "" {
		return cfg, errors.New("не задан топик: впиши \"topic\" в config.json")
	}
	cfg.UpdateRepo = strings.Trim(strings.TrimSpace(cfg.UpdateRepo), "/")
	if cfg.UpdateRepo == "" {
		cfg.UpdateRepo = defaultUpdateRepo
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 60
	}

	hadKey := strings.Contains(string(data), `"activities"`)
	var acts []Activity
	for _, a := range cfg.Activities {
		a.Title, a.Message, a.Menu = strings.TrimSpace(a.Title), strings.TrimSpace(a.Message), strings.TrimSpace(a.Menu)
		if a.Title == "" && a.Message == "" {
			continue
		}
		if a.Title == "" {
			a.Title = "Сбор"
		}
		if a.Message == "" {
			a.Message = a.Title
		}
		if a.Menu == "" {
			a.Menu = "Позвать: " + a.Title
		}
		if a.Priority < 1 || a.Priority > 5 {
			a.Priority = 4
		}
		acts = append(acts, a)
	}
	if !hadKey {
		acts = defaultActivities
	}
	cfg.Activities = acts
	return cfg, nil
}

func newClient(cfg Config, timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.Proxy != "" {
		if u, err := url.Parse(cfg.Proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}
