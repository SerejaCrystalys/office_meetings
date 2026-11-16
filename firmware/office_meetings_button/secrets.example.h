// Скопируй этот файл в secrets.h рядом и заполни.
// secrets.h в git не попадает (.gitignore).
#pragma once

// Любая сеть с интернетом, 2.4 ГГц, без страницы входа и без WPA2-Enterprise
const char* WIFI_SSID = "ИМЯ_СЕТИ";
const char* WIFI_PASS = "ПАРОЛЬ";

// Должны совпадать с настройками программы на компах
const char* NTFY_SERVER = "https://ntfy.sh";
const char* NTFY_TOPIC  = "СЮДА_ТОПИК_ИЗ_topic.txt";
const char* NTFY_TOKEN  = "";   // токен, если свой ntfy-сервер с авторизацией
