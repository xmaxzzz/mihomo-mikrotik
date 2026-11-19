# Хендофф: доработка xmaxzzz/mihomo-mikrotik (веб-панель, монитор, консоль, подписки)

Состояние на 2026-11-19. Вставить этот файл в новый чат Claude Code (рабочая папка `C:\Users\MAX\src\mihomo-mikrotik`).
Это хендофф **по коду**. Раскатка на роутеры — `D:\GITVS\mikrotik\MIGRATE-mihomo-web.md` и `HANDOFF-mihomo-web.md`.

---

## 1. Что это

Форк `wiktorbgu/mihomo-mikrotik` → **`github.com/xmaxzzz/mihomo-mikrotik`** (публичный).
Образ **`docker.io/xmaxzzz/mihomo-mikrotik`**, теги `:latest` и `:<sha7>`, платформы `linux/arm/v7` (главная — MikroTik L009),
`arm64`, `amd64`. Последний релиз: **`89b457b`** (CI зелёный, 2026-11-12).

Поверх upstream добавлено: веб-панель `mihomo-web` (Go, только stdlib) — управление серверами vless/hy2/mieru/naive и
подписками, монитор трафика, консоль `vpn`; naive (NaiveProxy) как отдельные процессы; UPX-сжатие бинарей под NAND 128 МБ;
патч sing-tun; правила шаблона против утечек UDP; CI под Docker Hub.

| Тег | Что |
|---|---|
| `9c08aac` | веб-панель, naive, UPX, CI (база upstream `36ad409`) |
| `9664aa5` | CI: `docker save --platform` (containerd store) |
| `4978eaa` | патч sing-tun: паника на exclude-uid в iptables-ветке (RouterOS) |
| `f183abf` | монитор, журнал, консоль `vpn`, `tun-in → REJECT` против утечки |
| `03d6d4d` | группа `UDP`: UDP через сервер с UDP, а не блок |
| `c843aa8` | консоль: ASCII-вывод, `serve` сначала занимает порт |
| `0a42c5b` | консоль на английском в терминале RouterOS |
| `89b457b` | подписки `https://…`, двуязычные ошибки API |

## 2. Репозиторий и окружение

- Клон: `C:\Users\MAX\src\mihomo-mikrotik`, `core.autocrlf=false` — **все файлы LF** (entrypoint.sh с CRLF не стартует).
  Проверять `file <файлы> | grep -i crlf` (не `grep -c $'\r'` — в подстановке Git Bash врёт).
- Remotes: `origin` = форк, `upstream` = wiktorbgu. **В upstream не пушить и PR туда не открывать.**
- Работа: прямо в `master` форка (так велось всё время), коммит → push → CI собирает и публикует.
  Коммиты заканчивать строкой `Co-Authored-By: Claude …` (по системной подсказке сессии).
- Go локально нет. Всё через контейнер:
  ```bash
  export MSYS_NO_PATHCONV=1
  docker run --rm -v "$(pwd -W)/web-panel:/src" -w /src -e CGO_ENABLED=0 golang:1.22-alpine \
    sh -c 'gofmt -w . && go vet ./... && go test -count=1 ./...'
  ```
- Docker Desktop после простоя бывает выключен — `Start-Process "C:\Program Files\Docker\Docker\Docker Desktop.exe"`.
- Кириллицу в файлах править **инструментом Edit/Write**. Heredoc в Git Bash и python-скрипт со stdin портят UTF-8.
  Для массовых замен — python-скрипт, записанный через Write в scratchpad, запуск `PYTHONUTF8=1 python -I script.py`.
- Сеть этой машины до Docker Hub / proxy.golang.org часто рвёт TLS: локальная сборка образа с ретраями
  (см. §6) или полагаться на CI и потом `docker pull` опубликованного тега.

## 3. Устройство

```
entrypoint.sh (upstream + патч) ──► mihomo (TUN/tproxy, шаблон /etc/mihomo_preset/template/default_config.yaml)
        └─► mihomo-web serve  ──► /etc/mihomo/web/servers.json (источник правды)
                │                 └─► servers.txt (base64 uri-list) ──► file-провайдер WEB (PUT /providers/proxies/WEB)
                ├─► naive: процесс на сервер, socks5 127.0.0.1:20800+, uid 2080 (исключён из TUN)
                ├─► монитор: опрос /connections, подписка на /logs, строки в stdout = журнал RouterOS
                ├─► подписки: скачивание, раскладка в серверы, фоновое обновление
                ├─► HTTP :8080 (Basic auth) — веб
                └─► unix-сокет /run/mihomo-web.sock (без пароля, root) — консоль `vpn`
```

### Файлы `web-panel/`

| Файл | Что |
|---|---|
| `main.go` | конфиг из env, `serve` (TCP-порт занимается первым!), роуты, хендлеры API, `apply()`, подписки (`refreshSub`, `subLoop`) |
| `store.go` | `Server`, servers.json, `Add/Update/Delete/Replace`, `Expanded()` (подписка + её серверы), `Active()`, рендер servers.txt, имена/порты |
| `sub.go` | `SubInfo`, `FetchSub` (UA `mihomo-web/<ver>`, прямо → через mixed-порт), разбор base64/текста, `Subscription-Userinfo`, `subDue` |
| `uri.go` | `ParseURI` (схемы → типы), `ProxyNames` (mieru даёт `имя:порт/протокол`), `NaiveArgs`, `WithName` |
| `naive.go` | `NaiveManager.Sync/Status/Logs`, процессы naive с uid |
| `mihomo.go` | клиент external-controller API |
| `monitor.go` | `Monitor`: скорость, соединения, новые запросы, `pollVia` (SELECTOR→AUTO→сервер), события RU/EN, `status:`-строки, поток `/logs` |
| `cli.go` | консоль: меню и разовые команды, строки `L(ru, en)` |
| `textout.go` | ASCII-режим: транслит + замены значков, фильтр stdout (для имён серверов по-русски) |
| `i18n.go` | `E/Ef` — двуязычные ошибки `uErr`, `errText(err, en)`, `wantEN(r)` по `Accept-Language: en` |
| `static/index.html` | весь веб в одном файле (CSS + JS), русский |
| `*_test.go` | uri, монитор (поддельный mihomo на httptest), подписки, транслит |

### API (все под Basic auth на :8080; без пароля через сокет)

`GET /api/state` · `POST /api/servers` (`{text, name}`, ссылки построчно или https-подписка) · `PUT/DELETE /api/servers/{id}` ·
`POST /api/servers/{id}/test|use|refresh` · `GET /api/servers/{id}/log` · `POST /api/test-all` · `POST /api/mode` ·
`POST /api/reorder` (только ID верхнего уровня) · `GET /api/export[?id=]` · `POST /api/import` ·
`GET /api/activity?recent=N` · `GET /api/connections` · `GET /api/logs?after=&limit=&q=` · `GET /healthz`.
Изменяющие запросы требуют заголовок `X-Mihomo-Web: 1` (CSRF).

### Шаблон mihomo (`mihomo-fs/etc/mihomo_preset/template/default_config.yaml`)

Правила:
```yaml
  - AND,((NETWORK,udp),(DST-PORT,443)),QUIC UDP 443
  - IN-NAME,tun-in,SELECTOR
  - AND,((IN-NAME,tun-in),(NETWORK,udp)),UDP   # TCP-only выход (naive): UDP через сервер с UDP
  - IN-NAME,tun-in,REJECT                       # что не ушло — блок, НЕ DIRECT
  - IN-NAME,mixed-in,SELECTOR
  - MATCH,GLOBAL
```
Группа `UDP` = url-test: `proxies: [REJECT]` + `use:` всех провайдеров, `exclude-type: "Socks5|Http|Direct"`.
Без живых — url-test берёт первый (REJECT). Entrypoint перезаливает шаблон в `/etc/mihomo/template` при старте, если отличается.

### Dockerfile (поверх upstream)

- Патч sing-tun: `go mod download` → копия в `/src/.sing-tun`, вырезан цикл `ExcludeUID` в `redirect_iptables.go`,
  `go mod edit -replace`. С проверкой «patch did not apply».
- Сборка `mihomo-web` (+ `go test`), обёртка `/usr/bin/vpn` (`vpn` без аргументов = `mihomo-web menu`).
- naive из релизов NaiveProxy, UPX для mihomo/mihomo-web/naive (`WITH_UPX=1`).

### Env (новые)

`WEB_ENABLE` `WEB_PORT=8080` `WEB_USER=admin` `WEB_PASSWORD` (=UI_SECRET) `NAIVE_BASE_PORT=20800` `NAIVE_UID=2080`
`NAIVE_DNS` `WEB_MONITOR_INTERVAL=5` `WEB_LOG_STATUS=300` `WEB_LOG_CONNECTIONS=false` `WEB_LOG_STREAM_LEVEL=info`
`WEB_SOCKET=/run/mihomo-web.sock` `WEB_SUB_INTERVAL=12` (часы). Таблица с описанием — `README-WEB.md`.

## 4. Поведение, которое легко сломать

- **RouterOS без nf_tables** → sing-tun идёт iptables-веткой (`DISABLE_NFTABLES=1` в образе). Десктопный docker — nftables/tproxy,
  поэтому TUN-баги RouterOS локально **не воспроизводятся**. Проверка — только на роутере.
- **`NAIVE_UID=0` нельзя**: naive не исключён из TUN → его соединения к своему серверу заворачиваются в mihomo → петля,
  шторм `198.19.0.1 --> <VPN-хост>`, CPU 70 %.
- **UDP при naive**: mihomo пропускает правило, если у прокси нет UDP. Без группы `UDP` и `REJECT` трафик падал в `MATCH,GLOBAL`=DIRECT
  мимо VPN (найдено на AP-115 по conntrack).
- **Терминал RouterOS** (WinBox, `/container shell`) выкидывает всё не-ASCII. Консоль по умолчанию: английский (`L(ru,en)`),
  `vpn --ru` / `--utf8` / `VPN_UTF8=1` — русский. Транслит пользователь назвал кривым — только для имён серверов.
  Строки монитора в stdout (журнал RouterOS) — только ASCII через `rosLine()`.
- Заглушка `«Нет серверов (напрямую)»` живёт в compatible-провайдере: `/proxies/<имя>` даёт 404 → `pollVia` берёт имя из `now`.
  В английском тексте — `enName()` → `no servers (direct)`.
- Внутренние соединения mihomo (inbound `Inner`) — `internal`, не утечка.
- `/logs?level=info` mihomo отдаёт поток независимо от `LOG_LEVEL` конфига (на этом журнал в вебе).
- API mihomo :9090 с `UI_SECRET` → 401 без `Authorization: Bearer`.
- Второй `mihomo-web serve` не должен трогать первый: порт занимается **до** naive и сокета (`c843aa8`).
- Подписка: серверы в `Sub.Items` (servers.json), те же ссылки сохраняют ID/имя/порт naive; ошибки сети без токена (`cleanNetErr`).
  RIXXX при неизвестном UA отдаёт generic base64 (naive+https, mierus, hysteria2, tt). `tt://` пропускается.
- CI-грабли: runner с containerd store — `docker save --platform linux/arm/v7`, размер по tar (`image inspect .Size`=0).
- `docker image inspect` на Desktop показывает ~50 МБ при реальных 22 МБ armv7-tar; rootfs ≈ 28 МБ.

## 5. Как проверять

1. Юнит-тесты (§2). Новые фичи — тест рядом (`monitor_test.go` с поддельным mihomo, `sub_test.go`).
2. Живой прогон без пересборки образа: взять опубликованный тег, подменить бинарь панели.
   ```bash
   export MSYS_NO_PATHCONV=1
   docker run --rm -v "$(pwd -W)/web-panel:/src" -w /src -e CGO_ENABLED=0 golang:1.22-alpine \
     go build -ldflags "-X main.version=test" -o /src/.mw-test .
   docker run -d --name mmw --cap-add NET_ADMIN -p 18081:8080 xmaxzzz/mihomo-mikrotik:89b457b
   # если entrypoint висит на `apk del nftables` (upstream, бывает на desktop): docker exec mmw sh -c 'kill $(pgrep -f "apk del")'
   docker cp web-panel/.mw-test mmw:/usr/bin/mihomo-web && docker exec mmw sh -c 'kill $(pgrep -f "mihomo-web serve")'
   # entrypoint перезапустит панель; дождаться: docker exec mmw vpn status | grep test
   ```
   Без `WEB_PASSWORD`/`UI_SECRET` веб открыт без пароля — удобно для браузера на `http://127.0.0.1:18081`.
   Трафик: в контейнере нет curl — `http_proxy=http://127.0.0.1:1080 wget -q -O /dev/null http://example.com/`.
   Подписка: в busybox образа нет httpd — отдать файл с хоста `python -I -m http.server 8099`,
   в контейнере ссылка `http://host.docker.internal:8099/sub/tok`.
3. Полная сборка образа локально (сеть рвётся — ретраи):
   ```bash
   for t in 1 2 3 4; do docker buildx build --platform linux/amd64 -t mmw:amd64 --load . > build.log 2>&1 && break; sleep 30; done
   ```
4. Релиз: push в `master` (изменения не только в `*.md`) → CI `build mihomo-mikrotik + web` (~17 мин, три платформы) →
   `docker buildx imagetools inspect xmaxzzz/mihomo-mikrotik:<sha7>` → обновить тег в
   `D:\GITVS\mikrotik\MIGRATE-mihomo-web.md` и `HANDOFF-mihomo-web.md` (sed по старому тегу) → дать команду пересоздания контейнера.

## 6. Роутеры (состояние, которое знаем)

| Объект | Роутер | Контейнер | Доступ |
|---|---|---|---|
| AP-115 Алые Паруса | 192.168.115.1, ROS 7.24.5 | наш образ; последний подтверждённый тег `0a42c5b`, обновление до `89b457b` выдано — не подтверждено | NetKnife: `butler-service`/`butler-rw`, ключ `~/.ssh/netknife_mt`, порт 10322 |
| Butler-222 | туннель 10.78.254.195 | wiktorbgu (2026-10-16), `tmpdir=/pull` на флеше | нет в NetKnife |
| mik_nick (офис 12) | 192.168.12.1, ROS 7.21.3 | **wiktorbgu** с самописной обёрткой `cmd=/user_sh/entrypoint-wrapper.sh`; падает signal 11 при остановке (см. §7.1); рядом `xmaxzzz/mihomo-uae` работает | нет в NetKnife; вход старым ключом запрещён классификатором |
| Mik_office | 192.168.5.1 | dnsproxy, не mihomo | `butler-rw` |

Контракт доступа к роутерам (обязателен): чтение — `butler-service`, запись — `butler-rw` только после явного «да» по объекту,
перед записью `/export` в `D:\GITVS\mikrotik\sites\<объект>\`. Классификатор авто-режима может запретить запись/вход —
**не обходить**, давать пользователю команды для WinBox. Подробно: память `mikrotik-butler-service-access-contract`.

Отправлены, но не подтверждены на AP-115: `delta6-leak-fixes.rsc` (`address-list-extra-time=1d`, IP вместо FWD-имён,
блок DoT 853). Слабый `WEB_PASSWORD=admin` на AP-115 — рекомендовано сменить.

## 7. Открытые задачи

1. **mik_nick: segfault обёртки.** Лог — сотни `[wrapper] stopping...` за 2 с, затем signal 11. Почти наверняка
   ловушка `trap '…; kill -TERM 0' TERM` → рекурсия (сигнал всей группе, включая себя). Ждём текст скрипта:
   `:put [/file get [find name~"entrypoint-wrapper.sh"] contents]` и лог до 17:02:45 (`!(message~"wrapper")`).
   Исправление: `trap 'trap - TERM INT; echo …; kill -TERM "$child"; wait "$child"; exit 0' TERM INT`, child = `$!`.
   Вариант лучше — перевести mik_nick на наш образ (подписки/монитор закрывают то, ради чего, вероятно, писалась обёртка).
2. **Проверить подписку flower на живом роутере** (`https://sub.flower.srv99.ru/sub/<token>`): naive-процесс, mieru TCP,
   hy2; UDP-профили mieru mihomo не умеет — должны отсеиваться при загрузке провайдера (в Dockerfile патч «skip invalid proxy»).
3. **Проверить группу `UDP` с живым UDP-сервером** при выбранном naive: в «Соединениях» UDP-строки «через DE», нет «напрямую».
4. **Шаблон новых объектов** `D:\GITVS\mikrotik\template\40-container.rsc` и `HANDOFF.md` §3.7 ставят wiktorbgu и `tmpdir=/pull`.
   Генератор `new-site.sh` байт-в-байт воспроизводит Butler-222 — менять осознанно.
5. **Upstream ушёл на 12 коммитов** вперёд от базы `36ad409` (последний `3356f2f`, в основном build/docs).
   Слить при следующей доработке entrypoint: `git merge upstream/master`, проверить конфликты в `Dockerfile`/`entrypoint.sh`.
6. Мелочи: строки naive в журнале RouterOS с русскими именами серверов (`naive.go` пишет имя как есть — прогнать через `rosLine`);
   в вебе колонки строк серверов подписки чуть «гуляют» (каждая строка — отдельный grid); Clash YAML / sing-box JSON подписки
   не поддерживаются (честная ошибка); отдельное включение/выключение серверов внутри подписки не сделано.
7. Риск UPX на других ядрах ARM: на AP-115 (7.24.5) работает. Если на каком-то роутере бинари падают сразу — собрать с `WITH_UPX=0`
   (`gh workflow run build-web.yml -f upx=0`), образ вырастет ~в 2 раза.

## 8. Где ещё смотреть

- `README-WEB.md` — пользовательская документация (env, мониторинг, консоль, подписки, UDP).
- `DEPLOY-L009.md` — развёртывание с нуля на L009.
- `HANDOFF.md` — исходный хендофф первой публикации (история, не актуален).
- Память Claude (`C:\Users\MAX\.claude\projects\D--GITVS\memory\`): `mihomo-mikrotik-fork-web-panel-dockerhub.md`,
  `mikrotik-ap115-alye-parusa.md`, `mikrotik-butler-service-access-contract.md`, `flower-srv99-vps-naive-mieru-panel.md`.
