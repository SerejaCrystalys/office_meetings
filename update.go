package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"
)

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
	req.Header.Set("User-Agent", "OfficeMeetings/"+version)
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
		if strings.EqualFold(a.Name, "OfficeMeetings.exe") {
			url, digest = a.URL, a.Digest
			break
		}
	}
	if url == "" {
		fail("В релизе " + rel.TagName + " нет файла OfficeMeetings.exe.")
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
