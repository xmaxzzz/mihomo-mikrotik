# Развёртывание на MikroTik L009 во ВНУТРЕННЮЮ память

Образ: `xmaxzzz/mihomo-mikrotik:latest` (Docker Hub, `linux/arm/v7`, бинари сжаты UPX).
Схема сети — как в вики оригинала: bridge `Bridge-Docker`, veth `MIHOMO` = `192.168.254.3`.

Допущения (поменяйте под объект):
- RouterOS **7.21+** (для 7.20 — см. примечание в шаге 6);
- LAN объекта `192.168.88.0/24`, WAN-интерфейсы в списке `WAN`;
- тест сначала на одном устройстве из списка `PROXY_CLIENTS`, потом — вся сеть.

---

## 0. Проверки до начала

```routeros
/system/resource print            # architecture-name: arm, free-hdd-space ≥ 45 MiB, free-memory ≥ 250 MiB
/system/package print             # нужен пакет container
/system/device-mode print         # нужен container=yes
/file print                       # нет ли старых docker/* от прошлых попыток
```

Места: образ в распакованном виде ≈ 30–35 МБ + данные `/etc/mihomo` < 1 МБ.
Слои при скачивании кладём в RAM (tmpfs), поэтому флешка нужна только под распакованный образ.

## 1. Пакет container и device-mode

Если `container` нет: скачать **Extra packages** для `arm` своей версии RouterOS с mikrotik.com/download,
залить `container-7.xx-arm.npk` в Files, затем:

```routeros
/system/package/apply-changes      ; на 7.18+ (иначе /system/reboot)
```

Если `container=no`:

```routeros
/system/device-mode/update mode=advanced container=yes
# в течение 5 минут: кратко нажать кнопку на корпусе или передёрнуть питание
```

## 2. RAM-диск для скачивания слоёв

```routeros
/disk add type=tmpfs tmpfs-max-size=120M slot=ramtmp
```

## 3. Сеть контейнера

```routeros
/interface/bridge add name=Bridge-Docker port-cost-mode=short
/ip/address add address=192.168.254.1/24 interface=Bridge-Docker
/interface/veth add name=MIHOMO address=192.168.254.3/24 gateway=192.168.254.1
/interface/bridge/port add bridge=Bridge-Docker interface=MIHOMO
/ip/firewall/nat add chain=srcnat src-address=192.168.254.0/24 out-interface-list=WAN action=masquerade comment=mihomo
```

## 4. Переменные

```routeros
/container/envs add list=MIHOMO key=UI_SECRET    value="ДЛИННЫЙ_СЕКРЕТ_API"
/container/envs add list=MIHOMO key=WEB_PASSWORD value="ПАРОЛЬ_ПАНЕЛИ"
/container/envs add list=MIHOMO key=LOG_LEVEL    value="warning"
/container/envs add list=MIHOMO key=TZ           value="Europe/Moscow"
```

`EXTERNAL_UI_URL` (zashboard) **не задаём** — экономим флешку, управление в панели `:8080`.
Если всё же нужен — лёгкая сборка 1.4 МБ:
`https://github.com/Zephyruso/zashboard/releases/latest/download/dist-cdn-fonts.zip`.

## 5. Реестр и загрузка

```routeros
/container/config set registry-url=https://registry-1.docker.io tmpdir=ramtmp ram-high=0
/container add remote-image=xmaxzzz/mihomo-mikrotik:latest interface=MIHOMO envlists=MIHOMO \
    root-dir=/docker/mihomo mount="src=/docker/mihomo-data,dst=/etc/mihomo" \
    dns=1.1.1.1,8.8.8.8 start-on-boot=yes logging=yes comment=mihomo
```

`/docker/...` — корень внутренней памяти (у L009 нет префикса `flash/`; проверить в `/file print`).
`dns=` — внешний DNS для самого контейнера (нужен naive и проверкам; fake-ip mihomo тут не использовать).

Ждать окончания распаковки (status `extracting` → `stopped`):

```routeros
/container print where comment=mihomo
/container start [find comment=mihomo]
```

## 6. Примечание для RouterOS 7.20

Вместо inline `mount=`: создать mount-объект и сослаться на него в контейнере
(посмотреть точные имена свойств: `/container/mounts add ?` и `/container add ?` в терминале).

## 7. Панель и серверы

- Панель: `http://192.168.254.3:8080/` — логин `admin`, пароль `WEB_PASSWORD`.
- «Добавить сервер» → вставить ссылки (`vless://`, `hy2://`, `mierus://`, `naive+https://`) или форма.
- «Проверить все» → у рабочих зелёная точка и мс.
- Режим: «Авто» (самый быстрый) или «Вручную» + «Использовать» у нужного сервера.
- «Скачать бэкап» — после настройки объекта сохранить JSON (в нём пароли).

## 8. Заворот трафика (сначала одно тестовое устройство)

```routeros
/routing/table add name=to_mihomo fib
/ip/route add dst-address=0.0.0.0/0 gateway=192.168.254.3 routing-table=to_mihomo comment=mihomo

/ip/firewall/address-list
add list=LOCAL_NETS address=10.0.0.0/8
add list=LOCAL_NETS address=172.16.0.0/12
add list=LOCAL_NETS address=192.168.0.0/16
add list=PROXY_CLIENTS address=192.168.88.50 comment="тестовый ноутбук"

/ip/firewall/mangle add chain=prerouting src-address-list=PROXY_CLIENTS dst-address-list=!LOCAL_NETS \
    dst-address-type=!local action=mark-routing new-routing-mark=to_mihomo passthrough=yes comment=mihomo

# FastTrack не должен обходить маркированный трафик
/ip/firewall/filter set [find action=fasttrack-connection] connection-mark=no-mark

# DNS клиентов из списка -> mihomo (fake-ip)
/ip/firewall/nat add chain=dstnat src-address-list=PROXY_CLIENTS protocol=udp dst-port=53 \
    action=dst-nat to-addresses=192.168.254.3 to-ports=53 comment=mihomo-dns
/ip/firewall/nat add chain=dstnat src-address-list=PROXY_CLIENTS protocol=tcp dst-port=53 \
    action=dst-nat to-addresses=192.168.254.3 to-ports=53 comment=mihomo-dns
```

Когда тест прошёл — вся сеть:

```routeros
/ip/firewall/address-list add list=PROXY_CLIENTS address=192.168.88.0/24 comment="весь LAN"
```

Внимание: если контейнер остановлен, клиенты из `PROXY_CLIENTS` теряют интернет (fail-closed).
Быстрый откат: `/ip/firewall/mangle disable [find comment=mihomo]` и `/ip/firewall/nat disable [find comment=mihomo-dns]`.

## 9. Проверка

```routeros
/container print where comment=mihomo                  # running
/log print where topics~"container"                     # "mihomo-web: http://..." и без fatal
/system/resource print                                  # free-memory, free-hdd-space
/log print where message~"status:|route"                # сводка монитора раз в 5 минут, смена сервера
/container shell [find comment=mihomo]                  # затем: vpn status  (или vpn — меню)
```

С тестового устройства:

```bash
curl https://ifconfig.me                                       # IP выходного VPN-сервера
nslookup example.com                                           # адрес из 198.18.x.x (fake-ip)
curl -u admin:ПАРОЛЬ http://192.168.254.3:8080/api/state       # "mihomo":{"ok":true,...}
```

## 10. Обновление образа

Данные (`servers.json`, выбор сервера) живут в `/docker/mihomo-data` и переживают пересоздание.

```routeros
/container stop [find comment=mihomo]
# дождаться status=stopped
/container remove [find comment=mihomo]
/container add remote-image=xmaxzzz/mihomo-mikrotik:latest interface=MIHOMO envlists=MIHOMO \
    root-dir=/docker/mihomo mount="src=/docker/mihomo-data,dst=/etc/mihomo" \
    dns=1.1.1.1,8.8.8.8 start-on-boot=yes logging=yes comment=mihomo
# после status=stopped:
/container start [find comment=mihomo]
```

Откат на конкретную сборку — тег по SHA: `remote-image=xmaxzzz/mihomo-mikrotik:<sha7>`.

## 11. Если что-то не так

| Симптом | Что делать |
|---|---|
| ошибка при распаковке / нет места | `/system/resource print` → free-hdd-space; удалить старые `/docker/*`; проверить, что `tmpdir=ramtmp` |
| ошибка скачивания слоёв | увеличить `tmpfs-max-size` до 150M; проверить `registry-url` |
| контейнер сразу `stopped` | `/log print where topics~"container"`; часто — синтаксис env |
| сервер в панели красный «не распознан» | ссылка не понялась mihomo → «Изменить», сверить формат |
| naive «нет связи» | «Журнал» у сервера в панели; проверить логин/пароль и что сервер — именно NaiveProxy (Caddy forwardproxy) |
| сайты не открываются у клиентов | FastTrack (шаг 8), DNS на 192.168.254.3, `/ip/route print where routing-table=to_mihomo` |
| нужна загрузка файлом без интернета | артефакт `mihomo-mikrotik-armv7.tar` из GitHub Actions → в Files → `/container add file=mihomo-mikrotik-armv7.tar ...` (на флешку нужно ≈ ×2 места на время распаковки) |
