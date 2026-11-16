// Office Meetings — кнопка сбора на ESP32-C3 SuperMini, отправка через ntfy (интернет).
// 1 нажатие — курилка, 2 — обед.
//
// Подключение:
//   GPIO3 ── кнопка ── GND
//   GPIO4 ── резистор 220–330 Ом ── (+) светодиод (−) ── GND
//
// Плата в IDE: ESP32C3 Dev Module, USB CDC On Boot: Enabled
//
// Индикация:
//   быстро мигает      — подключается к Wi-Fi / Wi-Fi пропал
//   тускло горит        — подключен, готов
//   ярко мигает N раз   — отправил (1 — курилка, 2 — обед)
//   частая дробь        — не отправил (кулдаун, нет сети, ошибка сервера)

#include <WiFi.h>
#include <WiFiClientSecure.h>
#include <HTTPClient.h>

// ====== НАСТРОЙКИ ======
// Wi-Fi, сервер и топик — в secrets.h (не попадает в git, пример — secrets.example.h)
#include "secrets.h"

const int BUTTON_PIN = 3;
const int LED_PIN    = 4;

const uint8_t LED_DIM  = 25;
const uint8_t LED_FULL = 255;

const unsigned long DEBOUNCE_MS     = 40;
const unsigned long PRESS_WINDOW_MS = 600;
const unsigned long COOLDOWN_MS     = 1000;
// =======================

WiFiClientSecure secureClient;
WiFiClient plainClient;

// Кнопка
bool lastReading = HIGH;
bool stableState = HIGH;
unsigned long lastChangeTime = 0;
int pressCount = 0;
unsigned long lastPressTime = 0;

// Отправка
unsigned long lastSendTime = 0;
bool everSent = false;

// Светодиод
int flashToggles = 0;
unsigned long flashInterval = 150;
unsigned long lastFlashTime = 0;
bool flashOn = false;
int ledValue = -1;

// Wi-Fi
bool wasConnected = false;

void setLed(int value) {
  if (value != ledValue) {
    analogWrite(LED_PIN, value);
    ledValue = value;
  }
}

void startFlash(int times, unsigned long interval) {
  flashToggles  = times * 2;
  flashInterval = interval;
  lastFlashTime = millis();
  flashOn = false;
  setLed(0);
}

void flashSent(int times) { startFlash(times, 180); }
void flashError()         { startFlash(5, 50); }

void updateLed() {
  unsigned long now = millis();
  if (flashToggles > 0) {
    if (now - lastFlashTime >= flashInterval) {
      lastFlashTime = now;
      flashOn = !flashOn;
      flashToggles--;
      setLed(flashOn ? LED_FULL : 0);
    }
    return;
  }
  if (WiFi.status() == WL_CONNECTED) setLed(LED_DIM);
  else setLed(((now / 120) % 2) ? LED_FULL : 0);
}

void updateWiFiLog() {
  bool connected = WiFi.status() == WL_CONNECTED;
  if (connected != wasConnected) {
    wasConnected = connected;
    if (connected) Serial.printf("Wi-Fi подключен, IP: %s\n", WiFi.localIP().toString().c_str());
    else           Serial.println("Wi-Fi пропал, переподключаюсь");
  }
}

bool sendNtfy(const char* title, const char* message) {
  if (WiFi.status() != WL_CONNECTED) {
    Serial.println("Нет Wi-Fi, сообщение не ушло");
    return false;
  }

  String body = String("{\"topic\":\"") + NTFY_TOPIC +
                "\",\"title\":\"" + title +
                "\",\"message\":\"" + message +
                "\",\"priority\":4}";
  String url = String(NTFY_SERVER) + "/";

  HTTPClient http;
  bool begun = url.startsWith("https")
             ? http.begin(secureClient, url)
             : http.begin(plainClient, url);
  if (!begun) {
    Serial.println("Не удалось начать HTTP-запрос");
    return false;
  }
  http.setTimeout(8000);
  http.addHeader("Content-Type", "application/json");
  if (strlen(NTFY_TOKEN) > 0) {
    http.addHeader("Authorization", String("Bearer ") + NTFY_TOKEN);
  }

  int code = http.POST(body);
  http.end();
  Serial.printf("ntfy: %s → HTTP %d\n", title, code);
  return code == 200;
}

void handlePresses(int count) {
  if (everSent && millis() - lastSendTime < COOLDOWN_MS) {
    Serial.println("Кулдаун, не спамь");
    flashError();
    return;
  }

  bool ok;
  if (count == 1)      ok = sendNtfy("Курилка", "🚬 Сбор на курилку!");
  else if (count == 2) ok = sendNtfy("Обед", "🍲 Сбор на обед!");
  else {
    Serial.printf("%d нажатий — такой команды нет\n", count);
    flashError();
    return;
  }

  if (ok) {
    lastSendTime = millis();
    everSent = true;
    flashSent(count);
  } else {
    flashError();
  }
}

void updateButton() {
  bool reading = digitalRead(BUTTON_PIN);
  if (reading != lastReading) {
    lastChangeTime = millis();
    lastReading = reading;
  }
  if (millis() - lastChangeTime > DEBOUNCE_MS && reading != stableState) {
    stableState = reading;
    if (stableState == LOW) {
      pressCount++;
      lastPressTime = millis();
    }
  }
  if (pressCount > 0 && stableState == HIGH &&
      millis() - lastPressTime > PRESS_WINDOW_MS) {
    handlePresses(pressCount);
    pressCount = 0;
  }
}

void setup() {
  Serial.begin(115200);
  delay(2000);
  Serial.printf("Старт. Подключаюсь к сети: %s\n", WIFI_SSID);
  Serial.printf("ntfy: %s/%s\n", NTFY_SERVER, NTFY_TOPIC);

  pinMode(BUTTON_PIN, INPUT_PULLUP);
  pinMode(LED_PIN, OUTPUT);

  // Сертификат сервера не проверяем — для кнопки сбора это допустимо
  secureClient.setInsecure();

  WiFi.mode(WIFI_STA);
  WiFi.setAutoReconnect(true);
  WiFi.begin(WIFI_SSID, WIFI_PASS);
  WiFi.setTxPower(WIFI_POWER_8_5dBm);
}

void loop() {
  updateWiFiLog();
  updateButton();
  updateLed();
}
