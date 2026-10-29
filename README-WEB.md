# mihomo-mikrotik + веб-панель серверов

Форк [wiktorbgu/mihomo-mikrotik](https://github.com/wiktorbgu/mihomo-mikrotik) с веб-управлением серверами:
**VLESS, Hysteria2, Mieru, Naive** (плюс любые ссылки, которые понимает mihomo: vmess, trojan, ss, tuic, anytls).
Всё остальное (SUB/SRV/AWG, tproxy/tun, шаблон конфига) работает как в оригинале.

## Как устроено

```
браузер ──► mihomo-web :8080 ──► /etc/mihomo/web/servers.json   (источник правды)
                │                └─► servers.txt (base64 uri-list) ──► file-провайдер WEB в mihomo
                │                                                       (PUT /providers/proxies/WEB, без рестарта)
                └─► naive (по процессу на сервер) socks5 127.0.0.1:20800+ ──► в mihomo как socks5
```

Провайдер `WEB` подключается первым в группы `AUTO / FALLBACK / MANUAL / LOADBALANCE`.
Нет ни одного включённого сервера → direct-заглушка «Нет серверов (напрямую)» (как в исходном образе).

## Переменные окружения

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `WEB_ENABLE` | `true` | включить панель и провайдер WEB |
| `WEB_PORT` | `8080` | порт панели |
| `WEB_USER` | `admin` | логин Basic auth |
| `WEB_PASSWORD` | = `UI_SECRET` | пароль; если оба пусты — панель **без пароля** (предупреждение в логе) |
| `NAIVE_BASE_PORT` | `20800` | первый локальный порт для naive |
| `NAIVE_UID` | `2080` | uid процессов naive; в TUN-режиме исключается из auto-route |
| `NAIVE_DNS` | пусто | DNS для резолва адреса naive-сервера мимо системного (если в контейнере fake-ip DNS) |
| `HEALTH_CHECK_URL` / `HEALTH_CHECK_TIMEOUT` | из образа | для кнопок «Проверить» |

Своя (не шаблонная) конфигурация (`CONFIG=...` без шаблона): провайдер WEB добавьте вручную:

```yaml
proxy-providers:
  WEB:
    type: file
    path: /etc/mihomo/web/servers.txt
    health-check: { enable: true, url: https://www.gstatic.com/generate_204, interval: 300, lazy: true }
```

## Форматы ссылок

```
vless://UUID@host:443?encryption=none&security=reality&sni=www.microsoft.com&fp=chrome&pbk=KEY&sid=ID&flow=xtls-rprx-vision&type=tcp#Имя
hy2://PASSWORD@host:443,20000-30000/?sni=host&obfs=salamander&obfs-password=X#Имя
mierus://USER:PASS@host?handshake-mode=HANDSHAKE_NO_WAIT&multiplexing=MULTIPLEXING_LOW&port=2012&protocol=TCP&profile=default#Имя
naive+https://USER:PASS@host:443?insecure-concurrency=2#Имя
naive+quic://USER:PASS@host:443#Имя
```

Mieru-ссылка с несколькими `port`/`protocol` даёт несколько прокси `Имя:порт/протокол`.
Naive — только TCP (UDP/QUIC-трафик клиентов через naive не идёт; правило `QUIC UDP 443` в шаблоне на это и рассчитано).

## Сборка

GitHub Actions (`.github/workflows/build-web.yml`) на каждый push в `master`:
- `xmaxzzz/mihomo-mikrotik:latest` и `:<sha7>` на Docker Hub для `arm/v7, arm64, amd64`
  (`provenance: false` — иначе RouterOS не тянет);
- бинари сжаты UPX (`WITH_UPX=1`): mihomo ~56→13 МБ, naive ~10.5→3.8 МБ, панель 5.7→1.6 МБ —
  образ помещается во внутреннюю NAND 128 МБ (L009);
- артефакт `mihomo-mikrotik-armv7.tar` — установка файлом.

Секреты репозитория: `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN` (Docker Hub PAT, Read & Write).

Локально:
```bash
docker buildx build --platform linux/arm/v7 --build-arg WITH_UPX=1 -t xmaxzzz/mihomo-mikrotik:test --load .
```

## Развёртывание

L009 во внутреннюю память — **[DEPLOY-L009.md](DEPLOY-L009.md)**.
