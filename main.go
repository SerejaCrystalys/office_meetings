// Office Meetings — уведомления от кнопки сбора для Windows (через ntfy).
// Подписывается на ntfy-топик; на каждый сбор открывает окно «Принять / Отклонить»,
// где видно, кто из коллег принял (✓), а кто отказался (✗).
// Живёт в трее. Настройки — в config.json рядом с exe (создаётся при первом запуске).
package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"

	"fyne.io/systray"
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
	defaultUpdateRepo = "SerejaCrystalys/office_meetings"
	staleAfter        = 2 * time.Minute // старые сообщения после переподключения не показываем
	silenceLimit      = 2 * time.Minute // нет даже keepalive — переподключаемся
)

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
		systray.SetTooltip("Office Meetings — на связи")
		if !online {
			systray.SetIcon(iconOn)
		}
	} else {
		statusItem.SetTitle("🔴 Нет связи: " + detail)
		systray.SetTooltip("Office Meetings — нет связи, переподключаюсь")
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
		lastItem.Enable() // клик открывает окно последнего сбора
	}
}

func newClientID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func cleanupOld() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	for i := 0; i < 20; i++ { // старую копию удаляем, когда она точно закрылась
		err := os.Remove(exe + ".old")
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func main() {
	arg := ""
	if len(os.Args) > 1 {
		arg = os.Args[1]
	}
	afterUpdate := arg == "--after-update"
	restarted := arg == "--restart"
	go cleanupOld()

	if !singleInstance(afterUpdate || restarted) {
		messageBox("Office Meetings", "Программа уже запущена — ищи значок в трее.", mbOK|mbIconInfo)
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		messageBox("Office Meetings", err.Error(), mbOK|mbIconError)
		return
	}
	clientID = newClientID()
	declineReasons = cfg.DeclineReasons

	systray.Run(func() {
		systray.SetIcon(iconOff)
		systray.SetTitle("Office Meetings")
		systray.SetTooltip("Office Meetings — подключаюсь")

		uiMu.Lock()
		statusItem = systray.AddMenuItem("⏳ Подключаюсь…", "")
		statusItem.Disable()
		lastItem = systray.AddMenuItem("Последний: пока тихо", "Открыть окно последнего сбора")
		lastItem.Disable()
		uiMu.Unlock()
		nameItem := systray.AddMenuItem("Ты: "+cfg.Name, "")
		nameItem.Disable()

		systray.AddSeparator()
		send := func(a Activity) {
			if err := startCall(cfg, a); err != nil {
				messageBox("Office Meetings", "Не удалось отправить: "+err.Error(), mbOK|mbIconError)
			}
		}
		if len(cfg.Activities) == 0 {
			empty := systray.AddMenuItem("Нет активностей в config.json", "")
			empty.Disable()
		}
		for _, a := range cfg.Activities {
			a := a
			item := systray.AddMenuItem(a.Menu, a.Message)
			go func() {
				for range item.ClickedCh {
					go send(a)
				}
			}()
		}

		systray.AddSeparator()
		test := systray.AddMenuItem("Проверить окно сбора", "")
		testPoll := systray.AddMenuItem("Проверить голосование за место", "")
		if len(cfg.LunchPlaces) == 0 {
			testPoll.Hide()
		}
		autostart := systray.AddMenuItemCheckbox("Запускать вместе с Windows", "", autostartEnabled())
		openCfg := systray.AddMenuItem("Открыть настройки (config.json)", "")
		reload := systray.AddMenuItem("Перезапустить (применить настройки)", "")
		systray.AddSeparator()
		verItem := systray.AddMenuItem("Версия "+version, "")
		verItem.Disable()
		upd := systray.AddMenuItem("Проверить обновления", "")
		quit := systray.AddMenuItem("Выход", "")

		if afterUpdate {
			notify("Office Meetings", "Обновлено до версии "+version+" 👍")
		}

		go subscribe(cfg)

		go func() {
			for {
				select {
				case <-test.ClickedCh:
					testCall(cfg, false)
				case <-testPoll.ClickedCh:
					testCall(cfg, true)
				case <-lastItem.ClickedCh:
					reopenLast(cfg)
				case <-autostart.ClickedCh:
					on := !autostart.Checked()
					if err := setAutostart(on); err != nil {
						messageBox("Office Meetings", "Не удалось изменить автозапуск: "+err.Error(), mbOK|mbIconError)
					} else if on {
						autostart.Check()
					} else {
						autostart.Uncheck()
					}
				case <-openCfg.ClickedCh:
					_ = exec.Command("notepad.exe", configPath()).Start()
				case <-reload.ClickedCh:
					if exe, err := os.Executable(); err == nil {
						if err := exec.Command(exe, "--restart").Start(); err == nil {
							systray.Quit()
							return
						}
					}
					messageBox("Office Meetings", "Не удалось перезапуститься", mbOK|mbIconError)
				case <-upd.ClickedCh:
					go checkUpdates(cfg)
				case <-quit.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()
	}, func() {})
}
