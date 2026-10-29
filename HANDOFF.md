# Хендофф: mihomo-mikrotik + веб-панель → Docker Hub xmaxzzz/mihomo-mikrotik

## Цель
Форк `wiktorbgu/mihomo-mikrotik` → `xmaxzzz/mihomo-mikrotik`, применить патч (веб-панель mihomo-web,
naive, UPX, CI под Docker Hub), собрать multi-arch образ и опубликовать `xmaxzzz/mihomo-mikrotik:latest`.
Целевое железо: MikroTik L009 (RouterOS 32-bit → `linux/arm/v7`), образ кладётся во ВНУТРЕННЮЮ NAND 128 МБ.

## Вход
`~/Downloads/mihomo-mikrotik-web.zip` → распаковать в `/tmp/mmw`:
`unzip -o ~/Downloads/mihomo-mikrotik-web.zip -d /tmp/mmw`
Внутри `mihomo-mikrotik-web/`: полный репо + `mihomo-web.patch` (относительно upstream master `36ad409`).

## Шаги

1. Предусловия: `gh auth status` (аккаунт xmaxzzz), `docker buildx version`, `go version` (≥1.22).
   Если чего-то нет — СТОП, сообщи что именно.

2. Форк и клон:
   ```bash
   gh repo view xmaxzzz/mihomo-mikrotik >/dev/null 2>&1 || gh repo fork wiktorbgu/mihomo-mikrotik --clone=false
   git clone https://github.com/xmaxzzz/mihomo-mikrotik.git ~/src/mihomo-mikrotik && cd ~/src/mihomo-mikrotik
   git remote add upstream https://github.com/wiktorbgu/mihomo-mikrotik.git 2>/dev/null; git fetch upstream
   git checkout -b feat/web-panel upstream/master
   ```

3. Патч:
   ```bash
   git apply --3way /tmp/mmw/mihomo-mikrotik-web/mihomo-web.patch && git status --short
   ```
   Ожидаемо изменены/добавлены: `Dockerfile`, `entrypoint.sh`, `README-WEB.md`, `DEPLOY-L009.md`, `HANDOFF.md`,
   `.github/workflows/build-web.yml`, `web-panel/*` (go.mod, main.go, store.go, uri.go, naive.go, mihomo.go,
   uri_test.go, static/index.html). Конфликт (upstream ушёл вперёд) → СТОП, пришли конфликтующие хунки.

4. Локальные проверки:
   ```bash
   (cd web-panel && go vet ./... && go test ./...)
   (cd web-panel && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags "-s -w" -o /tmp/mw . && ls -la /tmp/mw)
   bash -n entrypoint.sh
   ```
   Красное → СТОП.

5. Локальная сборка armv7 (проверка UPX и размера; если docker недоступен — пропусти, проверит CI):
   ```bash
   docker run --privileged --rm tonistiigi/binfmt --install arm >/dev/null
   docker buildx build --platform linux/arm/v7 -t mmw:armv7 --load .
   docker run --rm --platform linux/arm/v7 --entrypoint sh mmw:armv7 -c \
     'mihomo -v; mihomo-web version; naive --version; ls -la /usr/bin/mihomo /usr/bin/mihomo-web /usr/bin/naive; du -sxh /'
   docker image inspect mmw:armv7 --format '{{.Size}}' | awk '{printf "image MB: %.1f\n",$1/1048576}'
   ```
   Ожидаемо: три версии печатаются, mihomo ≈ 13 МБ, naive ≈ 4 МБ, mihomo-web ≈ 1.6 МБ, образ ≤ 45 МБ.
   Образ > 60 МБ или бинарь не стартует → СТОП, пришли вывод.

6. Смоук панели в контейнере (amd64, быстро):
   ```bash
   docker buildx build --platform linux/amd64 -t mmw:amd64 --load .
   docker run -d --name mmw --cap-add NET_ADMIN -e WEB_PASSWORD=t -e UI_SECRET=s -p 18080:8080 mmw:amd64
   sleep 8; docker logs mmw 2>&1 | tail -20
   curl -s -u admin:t http://127.0.0.1:18080/api/state | head -c 400; echo
   curl -s -u admin:t -H 'X-Mihomo-Web: 1' -H 'Content-Type: application/json' -X POST http://127.0.0.1:18080/api/servers \
     -d '{"text":"hy2://p@example.com:443#T1\nnaive+https://u:p@example.com:443#N1"}'; echo
   sleep 3; curl -s -u admin:t http://127.0.0.1:18080/api/state | head -c 800; echo
   docker exec mmw sh -c 'ps; base64 -d /etc/mihomo/web/servers.txt'
   docker rm -f mmw
   ```
   Ожидаемо: `"mihomo":{"ok":true`, оба сервера `loaded:true`, процесс naive в `ps`.
   Если tproxy/nft в docker не поднимается — это нормально для десктопного docker, главное `mihomo.ok=true`;
   если mihomo не стартует вовсе → СТОП с логом.

7. Секреты GitHub (Docker Hub). Попроси у Максима Docker Hub Personal Access Token (Read & Write),
   пусть вставит сам в prompt — НЕ выводи токен в лог/чат:
   ```bash
   gh secret set DOCKERHUB_USERNAME -R xmaxzzz/mihomo-mikrotik -b xmaxzzz
   gh secret set DOCKERHUB_TOKEN    -R xmaxzzz/mihomo-mikrotik      # интерактивный ввод
   gh api -X PUT repos/xmaxzzz/mihomo-mikrotik/actions/permissions -F enabled=true -f allowed_actions=all
   ```

8. Коммит и мерж в master форка:
   ```bash
   git add -A
   git commit -m "feat: web panel (vless/hy2/mieru/naive), UPX for 128MB NAND, Docker Hub CI"
   git push -u origin feat/web-panel
   gh pr create -R xmaxzzz/mihomo-mikrotik -B master -H feat/web-panel \
     -t "Web panel + naive + UPX + Docker Hub CI" -b "См. README-WEB.md / DEPLOY-L009.md"
   gh pr merge -R xmaxzzz/mihomo-mikrotik --squash --delete-branch
   ```
   PR создавать в СВОЙ форк (не в upstream wiktorbgu).

9. CI:
   ```bash
   sleep 10; RUN=$(gh run list -R xmaxzzz/mihomo-mikrotik -w "build mihomo-mikrotik + web" -L 1 --json databaseId -q '.[0].databaseId')
   gh run watch -R xmaxzzz/mihomo-mikrotik "$RUN" --exit-status
   ```
   Если run не стартовал (paths/Actions выключены) → `gh workflow run build-web.yml -R xmaxzzz/mihomo-mikrotik -f upx=1`.
   Красный → СТОП, пришли `gh run view "$RUN" --log-failed | tail -80`.

10. Проверка публикации:
    ```bash
    docker buildx imagetools inspect xmaxzzz/mihomo-mikrotik:latest
    docker pull --platform linux/arm/v7 xmaxzzz/mihomo-mikrotik:latest
    docker run --rm --platform linux/arm/v7 --entrypoint sh xmaxzzz/mihomo-mikrotik:latest -c 'mihomo -v; mihomo-web version; naive --version'
    ```
    Убедиться, что в манифесте есть `linux/arm/v7` и НЕТ записей `unknown/unknown` (attestation).
    Репозиторий на Docker Hub должен быть Public (по умолчанию так).

## Отчёт
- SHA мержа в master форка, ссылка на PR и на run;
- `xmaxzzz/mihomo-mikrotik:latest` и `:<sha7>` — digest для linux/arm/v7;
- размеры: armv7 image (MB), mihomo / mihomo-web / naive в образе;
- результат смоука (шаг 6) одной строкой;
- ссылка на артефакт `mihomo-mikrotik-armv7.tar`.

## Не делать
- Не пушить в upstream wiktorbgu, не открывать туда PR.
- Не менять логику entrypoint/шаблона сверх патча.
- Не печатать Docker Hub токен.
