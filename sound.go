package main

import (
	"bytes"
	"encoding/binary"
	"math"
	"sync"
	"syscall"
	"unsafe"
)

// Звук нового сбора — мультяшный «уи-ип, бо-оинг», синтезируется на лету (файлов в exe не таскаем).
// Режим — "sound" в config.json: fun (по умолчанию) | beep (системный сигнал) | off.
const (
	soundFun  = "fun"
	soundBeep = "beep"
	soundOff  = "off"
)

var soundMode = soundFun // из config.json, задаётся в main

var (
	winmm         = syscall.NewLazyDLL("winmm.dll")
	procPlaySound = winmm.NewProc("PlaySoundW")

	funOnce sync.Once
	funData []byte // держим в памяти: PlaySound с SND_ASYNC читает буфер, пока играет
)

const (
	sndAsync     = 0x0001
	sndNoDefault = 0x0002
	sndMemory    = 0x0004
)

// playAlert — звук нового сбора. Если проиграть не вышло — системный сигнал.
func playAlert() {
	switch soundMode {
	case soundOff:
		return
	case soundBeep:
		beep()
		return
	}
	funOnce.Do(func() { funData = funWAV() })
	r, _, _ := procPlaySound.Call(uintptr(unsafe.Pointer(&funData[0])), 0, sndMemory|sndAsync|sndNoDefault)
	if r == 0 {
		beep()
	}
}

// playSoft — тихий сигнал: итог голосования, свой сбор.
func playSoft() {
	if soundMode != soundOff {
		beep()
	}
}

const sampleRate = 22050

// funWAV — мультяшный сигнал: свисток вверх «уи-и-ип», потом две пружины «бо-о-оинг»
// (вторая повыше). ~1,8 с, 16 бит, моно.
func funWAV() []byte {
	const (
		length = 1.8
		volume = 0.9
		drive  = 1.8 // мягкое ограничение: громче на слух, но без хрипа
	)
	n := int(sampleRate * length)
	mix := make([]float64, n)

	// voice — один звук с меняющейся частотой: freq(t) и amp(t) от начала звука, t в секундах.
	voice := func(at, dur float64, freq, amp func(t float64) float64, wave func(ph float64) float64) {
		start := int(at * sampleRate)
		ph := 0.0
		for i := start; i < n && i < start+int(dur*sampleRate); i++ {
			t := float64(i-start) / sampleRate
			ph += 2 * math.Pi * freq(t) / sampleRate
			mix[i] += amp(t) * wave(ph)
		}
	}
	ramp := func(t, in, out, dur float64) float64 { // плавные края, без щелчков
		return math.Min(1, math.Min(t/in, (dur-t)/out))
	}

	// Свисток: 520 → ~1500 Гц за 0,4 с, с лёгким вибрато.
	voice(0, 0.42,
		func(t float64) float64 {
			x := math.Min(1, t/0.4)
			return 520 * math.Pow(2, 1.5*math.Pow(x, 0.8)) * (1 + 0.03*math.Sin(2*math.Pi*6*t))
		},
		func(t float64) float64 { return 0.55 * ramp(t, 0.02, 0.06, 0.42) },
		func(ph float64) float64 { return math.Sin(ph) + 0.1*math.Sin(2*ph) })

	// Пружина: частота сползает вниз и дрожит, дрожь быстро успокаивается.
	boing := func(at, f0, f1, dur float64) {
		voice(at, dur,
			func(t float64) float64 {
				base := f0 + (f1-f0)*t/dur
				return base * (1 + 0.35*math.Exp(-5*t)*math.Sin(2*math.Pi*16*t))
			},
			func(t float64) float64 { return math.Exp(-t/0.3) * ramp(t, 0.004, 0.05, dur) },
			func(ph float64) float64 { return math.Sin(ph) + 0.5*math.Sin(2*ph) + 0.25*math.Sin(3*ph) })
	}
	boing(0.5, 560, 330, 0.6)
	boing(1.08, 700, 410, 0.7)

	peak := 0.0
	for _, v := range mix {
		peak = math.Max(peak, math.Abs(v))
	}
	samples := make([]int16, n)
	for i, v := range mix {
		s := math.Tanh(drive*v/peak) / math.Tanh(drive)
		samples[i] = int16(s * volume * math.MaxInt16)
	}
	return wav(samples)
}

// wav — PCM 16 бит, моно, sampleRate.
func wav(samples []int16) []byte {
	var b bytes.Buffer
	data := len(samples) * 2
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + data))
	b.WriteString("WAVEfmt ")
	w(uint32(16))             // размер блока fmt
	w(uint16(1))              // PCM
	w(uint16(1))              // моно
	w(uint32(sampleRate))     // частота
	w(uint32(sampleRate * 2)) // байт в секунду
	w(uint16(2))              // байт на отсчёт
	w(uint16(16))             // бит на отсчёт
	b.WriteString("data")
	w(uint32(data))
	w(samples)
	return b.Bytes()
}
