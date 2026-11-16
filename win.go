package main

import (
	"errors"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procMessageBox   = user32.NewProc("MessageBoxW")
	procMessageBeep  = user32.NewProc("MessageBeep")
	procSetWindowPos = user32.NewProc("SetWindowPos")
	procSetFgWindow  = user32.NewProc("SetForegroundWindow")
	procCreateMutex  = kernel32.NewProc("CreateMutexW")
	procCloseHandle  = kernel32.NewProc("CloseHandle")
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

func beep() { procMessageBeep.Call(mbIconExclaim) }

// Простое окно-уведомление (запасной вариант, если нет WebView2).
func notify(title, text string) {
	beep()
	go messageBox(title, text, mbOK|mbIconInfo|mbTopmost|mbSetForeground)
}

// Окно поверх всех и на передний план.
func bringToFront(hwnd uintptr) {
	const hwndTopmost = ^uintptr(0) // -1
	const swpNoSize, swpNoMove, swpShowWindow = 0x1, 0x2, 0x40
	procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNoSize|swpNoMove|swpShowWindow)
	procSetFgWindow.Call(hwnd)
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

// После обновления/перезапуска старая копия ещё пару секунд закрывается — ждём её.
func singleInstance(wait bool) bool {
	if !wait {
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
const runName = "OfficeMeetings"
const runNameOld = "KnopkaSbor" // имя из версий до 1.3.0

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	if _, _, err = k.GetStringValue(runName); err == nil {
		return true
	}
	_, _, err = k.GetStringValue(runNameOld)
	return err == nil
}

func setAutostart(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	_ = k.DeleteValue(runNameOld)
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
