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

	procGetFgWindow     = user32.NewProc("GetForegroundWindow")
	procGetWindowThread = user32.NewProc("GetWindowThreadProcessId")
	procAttachThread    = user32.NewProc("AttachThreadInput")
	procBringToTop      = user32.NewProc("BringWindowToTop")
	procShowWindow      = user32.NewProc("ShowWindow")
	procIsIconic        = user32.NewProc("IsIconic")
	procFlashWindowEx   = user32.NewProc("FlashWindowEx")
	procCurThreadID     = kernel32.NewProc("GetCurrentThreadId")
	procKeybdEvent      = user32.NewProc("keybd_event")
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

const (
	hwndTopmost    = ^uintptr(0) // -1
	swpNoSize      = 0x1
	swpNoMove      = 0x2
	swpNoActivate  = 0x10
	swpShowWindow  = 0x40
	swRestore      = 9
	flashwStop     = 0
	flashwAll      = 0x3 // заголовок + кнопка на панели задач
	flashwTimer    = 0x4 // мигать, пока не остановим
	flashFastBlink = 250 // мс — быстрее обычного мигания Windows
	vkMenu         = 0x12
	keyeventfKeyUp = 0x2
)

// bringToFront — окно поверх всех и на передний план. Вызывать из потока окна.
// Windows не даёт фоновой программе забрать фокус (SetForegroundWindow молча
// не срабатывает), поэтому на время вызова подключаемся к потоку активного окна.
func bringToFront(hwnd uintptr) {
	if r, _, _ := procIsIconic.Call(hwnd); r != 0 {
		procShowWindow.Call(hwnd, swRestore)
	}
	procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNoSize|swpNoMove|swpShowWindow)
	fg, _, _ := procGetFgWindow.Call()
	if fg == hwnd {
		return
	}
	cur, _, _ := procCurThreadID.Call()
	fgThread, _, _ := procGetWindowThread.Call(fg, 0)
	if fg != 0 && fgThread != 0 && fgThread != cur {
		procAttachThread.Call(cur, fgThread, 1)
		defer procAttachThread.Call(cur, fgThread, 0)
	}
	procBringToTop.Call(hwnd)
	procSetFgWindow.Call(hwnd)
	if isForeground(hwnd) {
		return
	}
	// Запасной приём: Windows снимает блокировку фокуса после нажатия клавиши.
	// Alt «нажимаем» до вывода вперёд и «отпускаем» после — отпускание приходит уже
	// в наше окно, так что меню в чужой программе не открывается.
	procKeybdEvent.Call(vkMenu, 0, 0, 0)
	procSetFgWindow.Call(hwnd)
	procKeybdEvent.Call(vkMenu, 0, keyeventfKeyUp, 0)
}

// keepTopmost — напомнить, что окно поверх всех (другие «поверх всех» могли его перекрыть).
// Фокус не трогает.
func keepTopmost(hwnd uintptr) {
	procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNoSize|swpNoMove|swpNoActivate)
}

// isForeground — окно сейчас активное.
func isForeground(hwnd uintptr) bool {
	fg, _, _ := procGetFgWindow.Call()
	return fg == hwnd
}

// flash — мигать кнопкой на панели задач и заголовком (часто, без остановки) или перестать.
func flash(hwnd uintptr, on bool) {
	fi := struct {
		size    uint32
		hwnd    uintptr
		flags   uint32
		count   uint32
		timeout uint32
	}{hwnd: hwnd, flags: flashwStop}
	fi.size = uint32(unsafe.Sizeof(fi))
	if on {
		fi.flags, fi.timeout = flashwAll|flashwTimer, flashFastBlink
	}
	procFlashWindowEx.Call(uintptr(unsafe.Pointer(&fi)))
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
