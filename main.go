// KnopkaSbor — слушатель кнопки сбора для Windows (через ntfy).
// Подписывается на ntfy-топик и показывает окно поверх всех окон.
// Живёт в трее: статус, «позвать» с компа, проверка, настройки, выход.
// Настройки — в config.json рядом с exe (создаётся при первом запуске).
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"fyne.io/systray"
	"golang.org/x/sys/windows/registry"
)

//go:embed icon.ico
var iconOn []byte

//go:embed icon_off.ico
var iconOff []byte

// Подставляются при сборке:
//
//	-X main.version=1.2.0  -X main.defaultTopic=<топик из topic.txt>
var (
	version      = "1.0.0"
	defaultTopic = ""
)

const (
	defaultServer     = "https://ntfy.sh"
	defaultUpdateRepo = "SerejaCrystalys/office_meetings" // «ник/репозиторий» на GitHub, можно задать в config.json
	staleAfter        = 2 * time.Minute                   // старые сообщения после переподключения не показываем
	silenceLimit      = 2 * time.Minute                   // нет даже keepalive — переподключаемся
)

type Config struct {
	Server     string `json:"server"`
	Topic      string `json:"topic"`
	Token      string `json:"token"`
	Proxy      string `json:"proxy"`
	UpdateRepo string `json:"update_repo"`
}

type ntfyEvent struct {
	ID      string `json:"id"`
	Time    int64  `json:"time"`
	Event   string `json:"event"`
	Title   string `json:"title"`
	Message string `json:"message"`
}

// ---------- WinAPI ----------

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procMessageBox  = user32.NewProc("MessageBoxW")
	procMessageBeep = user32.NewProc("MessageBeep")
	procCreateMutex = kernel32.NewProc("CreateMutexW")
	procCloseHandle = kernel32.NewProc("CloseHandle")
)

const (
	mbOK            = 0x0
	mbIconInfo      = 0x40
	mbIconError     = 0x10
	mbIconExclaim   = 0x30
	mbTopmost       = 0x40000
	mbSetForeground = 0x10000
	errAlreadyExist = 183
)

func messageBox(title, text string, flags uintptr) {
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(text)
	procMessageBox.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), flags)
}

func notify(title, text string) {
	procMessageBeep.Call(mbIconExclaim)
	go messageBox(title, text, mbOK|mbIconInfo|mbTopmost|mbSetForeground)
}

func tryMutex() bool {
	name, _ := syscall.UTF16PtrFromString("Local\\KnopkaSborListener")
	h, _, err := procCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if err == syscall.Errno(errAlreadyExist) {
		procCloseHandle.Call(h)
		return false
	}
	return h != 0
}

// После обновления старая копия ещё пару секунд закрывается — ждём её.
func singleInstance(afterUpdate bool) bool {
	if !afterUpdate {
		return tryMutex()
	}
	for i := 0; i < 50; i++ {
		if tryMutex() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// ---------- Автозапуск ----------

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runName = "KnopkaSbor"

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runName)
	return err == nil
}

func setAutostart(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		err = k.DeleteValue(runName)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue(runName, `"`+exe+`"`)
}

// ---------- Обновления с GitHub ----------

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

func parseVer(s string) [3]int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	var v [3]int
	for i, p := range strings.SplitN(s, ".", 3) {
		n, _ := strconv.Atoi(strings.TrimFunc(p, func(r rune) bool { return r < '0' || r > '9' }))
		v[i] = n
	}
	return v
}

func newer(a, b string) bool { // a новее b?
	va, vb := parseVer(a), parseVer(b)
	for i := 0; i < 3; i++ {
		if va[i] != vb[i] {
			return va[i] > vb[i]
		}
	}
	return false
}

var updating sync.Mutex

func checkUpdates(cfg Config) {
	if !updating.TryLock() {
		return
	}
	defer updating.Unlock()

	fail := func(msg string) {
		messageBox("Обновление", msg, mbOK|mbIconError)
	}
	if cfg.UpdateRepo == "" {
		fail("Не указан репозиторий для обновлений.\nВпиши \"update_repo\": \"ник/репозиторий\" в config.json.")
		return
	}

	client := newClient(cfg, 60*time.Second)
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/"+cfg.UpdateRepo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "KnopkaSbor/"+version)
	resp, err := client.Do(req)
	if err != nil {
		fail("Не удалось проверить обновления:\n" + err.Error())
		return
	}
	var rel ghRelease
	err = json.NewDecoder(resp.Body).Decode(&rel)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || err != nil {
		fail(fmt.Sprintf("GitHub ответил HTTP %d.\nПроверь, что репозиторий %s публичный и в нём есть релиз.", resp.StatusCode, cfg.UpdateRepo))
		return
	}

	if !newer(rel.TagName, version) {
		messageBox("Обновление", "У тебя последняя версия: "+version, mbOK|mbIconInfo)
		return
	}

	var url, digest string
	for _, a := range rel.Assets {
		if strings.EqualFold(a.Name, "KnopkaSbor.exe") {
			url, digest = a.URL, a.Digest
		}
	}
	if url == "" {
		fail("В релизе " + rel.TagName + " нет файла KnopkaSbor.exe.")
		return
	}

	exe, err := os.Executable()
	if err != nil {
		fail(err.Error())
		return
	}
	newPath, oldPath := exe+".new", exe+".old"

	resp, err = client.Get(url)
	if err != nil {
		fail("Не удалось скачать обновление:\n" + err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Sprintf("Скачивание: HTTP %d", resp.StatusCode))
		return
	}
	f, err := os.Create(newPath)
	if err != nil {
		fail("Не удалось записать файл:\n" + err.Error())
		return
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	f.Close()
	if err != nil {
		os.Remove(newPath)
		fail("Обрыв при скачивании:\n" + err.Error())
		return
	}
	if want := strings.TrimPrefix(digest, "sha256:"); want != "" && !strings.EqualFold(want, hex.EncodeToString(h.Sum(nil))) {
		os.Remove(newPath)
		fail("Файл обновления повреждён (не сошлась контрольная сумма). Попробуй ещё раз.")
		return
	}

	os.Remove(oldPath)
	if err := os.Rename(exe, oldPath); err != nil {
		os.Remove(newPath)
		fail("Не удалось заменить программу:\n" + err.Error())
		return
	}
	if err := os.Rename(newPath, exe); err != nil {
		os.Rename(oldPath, exe)
		fail("Не удалось поставить новую версию:\n" + err.Error())
		return
	}
	if err := exec.Command(exe, "--after-update").Start(); err != nil {
		fail("Новая версия установлена, но не запустилась:\n" + err.Error())
		return
	}
	systray.Quit()
}

// ---------- Конфиг ----------

func configPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}

func loadConfig() (Config, error) {
	path := configPath()
	cfg := Config{Server: defaultServer, Topic: defaultTopic, UpdateRepo: defaultUpdateRepo}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		out, _ := json.MarshalIndent(cfg, "", "  ")
		_ = os.WriteFile(path, out, 0644)
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("ошибка в config.json: %w", err)
	}
	cfg.Server = strings.TrimRight(strings.TrimSpace(cfg.Server), "/")
	cfg.Topic = strings.TrimSpace(cfg.Topic)
	if cfg.Server == "" {
		cfg.Server = defaultServer
	}
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

// ---------- Трей ----------

var (
	uiMu       sync.Mutex
	statusItem *systray.MenuItem
	lastItem   *systray.MenuItem
	online     = false
)

func setOnline(on bool, detail string) {
	uiMu.Lock()
	defer uiMu.Unlock()
	if statusItem == nil {
		return
	}
	if on {
		statusItem.SetTitle("🟢 На связи: " + detail)
		systray.SetTooltip("Кнопка сбора — на связи")
		if !online {
			systray.SetIcon(iconOn)
		}
	} else {
		statusItem.SetTitle("🔴 Нет связи: " + detail)
		systray.SetTooltip("Кнопка сбора — нет связи, переподключаюсь")
		if online {
			systray.SetIcon(iconOff)
		}
	}
	online = on
}

func setLast(text string) {
	uiMu.Lock()
	defer uiMu.Unlock()
	if lastItem != nil {
		lastItem.SetTitle(text)
	}
}

// ---------- ntfy ----------

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
		if len(msg) > 60 {
			msg = msg[:60] + "…"
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

	u := fmt.Sprintf("%s/%s/json", cfg.Server, url.PathEscape(cfg.Topic))
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
		title := ev.Title
		if title == "" {
			title = "Кнопка сбора"
		}
		stamp := sent.Format("15:04")
		setLast(fmt.Sprintf("Последний: %s в %s", title, stamp))
		notify(title, fmt.Sprintf("%s\n%s", ev.Message, stamp))
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("сервер закрыл соединение")
}

func publish(cfg Config, title, message string) error {
	body, _ := json.Marshal(map[string]any{
		"topic":    cfg.Topic,
		"title":    title,
		"message":  message,
		"priority": 4,
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

// ---------- main ----------

func main() {
	afterUpdate := len(os.Args) > 1 && os.Args[1] == "--after-update"
	if exe, err := os.Executable(); err == nil {
		go func() { // старую копию удаляем, когда она точно закрылась
			for i := 0; i < 20; i++ {
				err := os.Remove(exe + ".old")
				if err == nil || errors.Is(err, os.ErrNotExist) {
					return
				}
				time.Sleep(500 * time.Millisecond)
			}
		}()
	}
	if !singleInstance(afterUpdate) {
		messageBox("Кнопка сбора", "Слушатель уже запущен — ищи значок в трее.", mbOK|mbIconInfo)
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		messageBox("Кнопка сбора", err.Error(), mbOK|mbIconError)
		return
	}

	systray.Run(func() {
		systray.SetIcon(iconOff)
		systray.SetTitle("Кнопка сбора")
		systray.SetTooltip("Кнопка сбора — подключаюсь")

		uiMu.Lock()
		statusItem = systray.AddMenuItem("⏳ Подключаюсь…", "")
		statusItem.Disable()
		lastItem = systray.AddMenuItem("Последний: пока тихо", "")
		lastItem.Disable()
		uiMu.Unlock()

		systray.AddSeparator()
		callSmoke := systray.AddMenuItem("🚬 Позвать на курилку", "")
		callLunch := systray.AddMenuItem("🍲 Позвать на обед", "")
		systray.AddSeparator()
		test := systray.AddMenuItem("Проверить уведомление", "")
		autostart := systray.AddMenuItemCheckbox("Запускать вместе с Windows", "", autostartEnabled())
		openCfg := systray.AddMenuItem("Открыть настройки (config.json)", "")
		systray.AddSeparator()
		verItem := systray.AddMenuItem("Версия "+version, "")
		verItem.Disable()
		upd := systray.AddMenuItem("Проверить обновления", "")
		quit := systray.AddMenuItem("Выход", "")

		if afterUpdate {
			notify("Кнопка сбора", "Обновлено до версии "+version+" 👍")
		}

		go subscribe(cfg)

		send := func(title, msg string) {
			if err := publish(cfg, title, msg); err != nil {
				messageBox("Кнопка сбора", "Не удалось отправить: "+err.Error(), mbOK|mbIconError)
			}
		}

		go func() {
			for {
				select {
				case <-callSmoke.ClickedCh:
					go send("Курилка", "🚬 Сбор на курилку!")
				case <-callLunch.ClickedCh:
					go send("Обед", "🍲 Сбор на обед!")
				case <-test.ClickedCh:
					notify("Проверка", "Уведомления работают 👍")
				case <-autostart.ClickedCh:
					on := !autostart.Checked()
					if err := setAutostart(on); err != nil {
						messageBox("Кнопка сбора", "Не удалось изменить автозапуск: "+err.Error(), mbOK|mbIconError)
					} else if on {
						autostart.Check()
					} else {
						autostart.Uncheck()
					}
				case <-upd.ClickedCh:
					go checkUpdates(cfg)
				case <-openCfg.ClickedCh:
					_ = exec.Command("notepad.exe", configPath()).Start()
				case <-quit.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()
	}, func() {})
}
