# fencebreaker — справочник по конфигурации

Полное руководство по настройке fencebreaker через UCI (`/etc/config/fencebreaker`)
и LuCI. Схема опций соответствует `docs/CONTRACTS.md` §1; значения по умолчанию
и правила валидации — как их применяет демон (`fencebreakerd`).

## Содержание

1. [Где живёт конфигурация](#где-живёт-конфигурация)
2. [Раздел `global`](#раздел-global)
3. [Раздел `tenant`](#раздел-tenant)
4. [Разделы `list`](#разделы-list)
5. [Примеры конфигурации](#примеры-конфигурации)
6. [Соответствие LuCI](#соответствие-luci)
7. [Диагностика конфигурации](#диагностика-конфигурации)

---

## Где живёт конфигурация

- Конфигурация — один файл UCI: **`/etc/config/fencebreaker`**. Он создаётся
  при первой установке из поставляемого примера
  (`/usr/share/fencebreaker/fencebreaker.conf`) и **не перезаписывается при
  обновлении пакетов** — обновляются только программные файлы; конфигурация и
  кэш списков в `/etc/fencebreaker/lists/` сохраняются.
- **Применение изменений:**
  - в LuCI — кнопка *Save & Apply* (Сохранить и применить): демон перечитывает
    конфигурацию автоматически (через `ucitrack` → procd reload → SIGHUP);
    службы CoreDNS/sing-box перезапускаются только если их конфигурация
    реально изменилась;
  - при ручном редактировании файла или через `uci set` в шелле нужен
    рестарт службы:

    ```sh
    /etc/init.d/fencebreaker restart
    ```

- Runtime-состояние (генерируемые конфиги, файлы статуса) живёт в
  `/var/run/fencebreaker/` и пересоздаётся демоном само.
- Максимальное число тенантов — **32**. Порядковые индексы тенантов (с 1)
  определяют номера таблиц маршрутизации (`100 + N`), fwmark (`0x1000 + N−1`)
  имена nft-наборов (`t{N}_…`) — детали в `docs/CONTRACTS.md` §1.

---

## Раздел `global`

Секция `config fencebreaker 'global'` — ровно одна, глобальные настройки
демона. В LuCI этот раздел не редактируется — только через UCI.

| Опция | Тип | По умолчанию | Назначение |
|---|---|---|---|
| `enabled` | bool | `1` | Главный выключатель демона. |
| `geodata_url` | string | `https://github.com/v2fly/geoip/releases/latest/download/geoip.dat` | URL базы GeoIP (geoip.dat) для `include_geoip`/`exclude_geoip`. |
| `geodata_refresh` | int (часы) | `168` | Период обновления GeoIP-базы; `0` — только вручную. |
| `lists_refresh` | int (часы) | `24` | Период обновления URL-списков по умолчанию. |
| `coredns_port` | int | `1053` | Порт единственного CoreDNS-инстанса: сюда уходит перехваченный DNS клиентов тенантов. |
| `ttl_min` | int (сек) | `300` | Нижняя граница TTL обученных адресов в nft-наборах. |
| `ttl_max` | int (сек) | `86400` | Верхняя граница TTL обученных адресов. |
| `dns_refresh_on_remain_pct` | int (%) | `0` | Продлевать timeout элемента повторным обновлением, когда остаток времени меньше N% исходного; `0` — не продлевать. |
| `max_set_size` | int | `65536` | Лимит обученного набора на тенанта; при превышении вытесняются самые старые элементы. |
| `resync_interval` | int (сек) | `60` | Период ресинхронизации зеркал nft-наборов в плагине. |

---

## Раздел `tenant`

Каждая секция `config tenant` — один тенант маршрутизации. Разделы можно
создавать анонимно (`config tenant`) или именованными (`config tenant 'my'`).

### Общие

| Опция | Тип | По умолчанию | Назначение |
|---|---|---|---|
| `id` | string | имя секции | Уникальный идентификатор: строчные буквы `a-z`, цифры, `_`, `-`; первый символ — буква или цифра. На него ссылаются `dnsrule`. LuCI генерирует `tenantN` для новых тенантов. |
| `name` | string | имя секции | Отображаемое имя (в LuCI и статусе). |
| `enabled` | bool | `1` | Включён ли тенант. Требования к каналу проверяются только для включённых. |
| `priority` | int | `100` | Меньшее число — важнее. Учитывается при конфликте полных хостов (`full_hosts`) между тенантами. |
| `fallback` | `main` \| `strict` | `main` | Поведение при падении канала: `main` — трафик тенанта идёт по основной таблице (обычный роутинг); `strict` — трафик тенанта блокируется до восстановления канала. |
| `routing_mode` | `select` \| `invert` | `select` | Режим маршрутизации, см. ниже. |

**`routing_mode` подробно.**

- **`select`** — через канал тенанта идёт *только перечисленное*:
  `include_subnets`, адреса из DNS-ответов для `include_domains` (суффиксы) и
  `include_lists`, подсети стран `include_geoip`, плюс весь трафик
  `full_hosts`. Исключения (`exclude_*`) сильнее включений: адреса доменов из
  `exclude_domains` никогда не попадут в канал, даже если совпала
  include-подсеть.
- **`invert`** — через канал идёт *всё, кроме исключений*: любые домены,
  кроме `exclude_domains`/`exclude_lists`, плюс все подсети, кроме
  `exclude_subnets`/`exclude_geoip`. Опции `include_*` в этом режиме не
  используются. Типичный сценарий — «всё через туннель, кроме локальных
  банков и сайтов своей страны» (пример 3 ниже).

### Канал (egress)

| Опция | Тип | По умолчанию | Назначение |
|---|---|---|---|
| `egress_mode` | `interface` \| `vless` | `interface` | Тип исходящего канала. |
| `egress_interface` | string | — (обязателен при `egress_mode=interface`) | Имя **устройства** (не uci-сети): физический порт (`eth1`), мост или туннель (`wg0`). В LuCI список берётся из сетевых устройств роутера. |

**VLESS** (`egress_mode=vless`) требует установленный `sing-box`
(пакет опционален: `apk add sing-box` — или `opkg install sing-box` на
старых релизах; установщик `scripts/install-openwrt.sh --with-singbox` ставит
его автоматически). Без бинарника служба sing-box просто не стартует.
Поддерживается только TCP-транспорт (TLS/REALITY).

Рекомендуемый способ настройки — одна строка `vless_uri`:

```
vless://<uuid>@<host>:<port>?<параметры>#<имя>
```

| Параметр URI | Значение |
|---|---|
| `security` | `reality` \| `tls` \| `none`; отсутствует/пусто = `none`; регистр не важен |
| `pbk` | public key — **обязателен** для `security=reality` |
| `sni` | имя сервера — **обязателен** для `security=tls` |
| `sid` | reality short id |
| `fp` | отпечаток клиента; по умолчанию `chrome` (когда security != none) |
| `flow` | например `xtls-rprx-vision` |
| `type` | только `tcp` (пусто = `tcp`); любое другое значение — ошибка |

Фрагмент `#имя` в конце ссылки игнорируется. Демон разбирает строку сам.

| Опция (legacy) | Тип | По умолчанию | Назначение |
|---|---|---|---|
| `vless_uri` | string | — | Ссылка-поделиться; **если задана, перекрывает поля ниже**. |
| `vless_uuid` | string | — | UUID клиента (обязателен вместе с server/port). |
| `vless_server` | string | — | Адрес сервера (обязателен). |
| `vless_port` | int | `0` | Порт 1–65535 (обязателен). |
| `vless_flow` | string | — | Flow, например `xtls-rprx-vision`. |
| `vless_security` | `reality` \| `tls` \| `none` | `reality` | Тип защиты. |
| `vless_sni` | string | — | SNI (обязателен для `tls`). |
| `vless_fp` | string | `chrome` | Отпечаток клиента. |
| `vless_pbk` | string | — | Public key (обязателен для `reality`). |
| `vless_sid` | string | — | Reality short id. |

Легаси-поля используются только при пустом `vless_uri` и существуют для
совместимости; для нового туннеля удобнее одна строка-поделиться из
клиентского приложения.

### Ресурсы

Четыре пары списков: включаемые (только для `select`) и исключаемые.

| Опция | Формат записи | Назначение |
|---|---|---|
| `include_subnets` | CIDRv4, по одной: `203.0.113.0/24`, `5.6.7.8/32` | Подсети, трафик которых направляется в канал. |
| `include_domains` | суффиксы доменов: `example.com` | Адреса из DNS-ответов этих доменов (включая поддомены) попадают в канал. |
| `include_geoip` | коды стран ISO 3166-1 alpha-2: `ru`, `nl` | Подсети стран из GeoIP-базы. |
| `include_lists` | имена секций `list` | Содержимое списков-подписок/ручных списков. |
| `exclude_subnets` | CIDRv4 | Никогда не направляются в канал. |
| `exclude_domains` | суффиксы доменов | Адреса из DNS-ответов этих доменов всегда уходят в исключение (сильнее включений). |
| `exclude_geoip` | коды стран | Никогда не направляются в канал. |
| `exclude_lists` | имена секций `list` | Исключаемые списки. |

Семантика по режимам:

- **select**: в канал идут только include-ресурсы; exclude-ресурсы
  вычитаются из них (исключение сильнее совпавшей include-подсети).
- **invert**: в канал идёт весь DNS-обучаемый трафик, **кроме** exclude-ресурсов
  (именно исключения обязаны «накрывать» то, что не должно идти в канал);
  include-опции игнорируются.

### Клиенты

| Опция | Формат | По умолчанию | Назначение |
|---|---|---|---|
| `networks` | uci-имена сетей (`lan`, `guest`), имена устройств (`br-guest`) или CIDR (`192.168.20.0/24`) | — (обязателен, минимум одна запись) | Чьи клиенты обслуживаются тенантом. Сети одного включённого тенанта не должны пересекаться с сетями другого — такая конфигурация не применится (ошибка валидации); LuCI предупреждает при сохранении. |
| `excluded_hosts` | IPv4-адреса | — | Хосты из клиентских сетей, не затрагиваемые тенантом: их трафик и DNS идут обычным порядком (DNS — в dnsmasq). |
| `full_hosts` | IPv4-адреса | — | Хосты, **весь** трафик которых направляется в канал тенанта без DNS-обучения. При совпадении хоста у нескольких тенантов побеждает меньший `priority`. |

### DNS

| Опция | Тип | По умолчанию | Назначение |
|---|---|---|---|
| `dns_hijack` | bool | `1` | Перехват DNS (TCP/UDP порт 53) клиентов тенанта на локальный CoreDNS (`coredns_port`). Отключать только осознанно: без перехвата обучение по доменам работать перестанет — адреса изученных доменов перестанут попадать в nft-наборы. |
| `dns_via_tunnel` | bool | `1` | Upstream-запросы DNS тенанта идут через его канал (исходящие сокеты помечаются fwmark тенанта, SO_MARK). Bootstrap-резолв имён upstream-серверов и получение сертификата DNSCrypt идут без метки. |
| `dns_keepalive` | bool | `0` | Продление адресов доменов: фоновый перезапрос изученных доменов до истечения их TTL, чтобы маршрутизация не прерывалась (см. ниже). |
| `dns_upstream` | список адресов | — | DNS-серверы тенанта, см. ниже. |
| `upstream_policy` | `sequential` \| `random` | `sequential` | Порядок выбора сервера из списка: последовательно / случайный из здоровых. |

**`dns_keepalive` — кому нужно.** Обучение пассивно: выученный адрес живёт
ровно clamp(TTL) с момента первого изучения, и после истечения маршрутизация
этого домена пропадает, пока клиент сам не сделает новый DNS-запрос. Если это
недопустимо (долгие соединения, headless-устройства), включайте
`dns_keepalive`: демон фоново перезапрашивает изученные домены, когда до
истечения осталось меньше max(60 с, 25%·timeout), и продлевает элементы.
Записи удаляются при NXDOMAIN/пустом ответе или трёх подряд ошибках
upstream. Реестр доменов переживает рестарт службы (не ребут).

**`dns_upstream` — форматы адресов.** По одной записи в списке; допустимы
следующие формы (везде, где указан хост, можно писать и IP, и доменное имя;
IPv6 без порта — «голым» литералом, с портом — в квадратных скобках):

| Форма | Значение |
|---|---|
| `1.2.3.4`, `2a10:50c0::1:ff` | обычный DNS (UDP), порт 53 |
| `1.2.3.4:53`, `[2a10:50c0::1:ff]:53` | UDP с портом |
| `udp://host[:port]` | UDP по имени хоста |
| `tcp://host[:port]` | DNS поверх TCP |
| `tls://host[:port]` | DNS-over-TLS (порт по умолчанию 853) |
| `https://host[:port][/dns-query]` | DNS-over-HTTPS |
| `h3://host[:port][/dns-query]` | DoH, принудительно HTTP/3 (без отката к h2) |
| `quic://host[:port]` | DNS-over-QUIC (RFC 9250) |
| `sdns://<base64url>` | DNS Stamp: DNSCrypt или DoH |

Проверяется только синтаксис (порт 1–65535, допустимые схемы, корректное
имя хоста); достижимость — на стороне плагина.

- **Несколько адресов = резервирование.** Неответивший сервер исключается из
  выбора на 10 с, срок удваивается с каждой следующей подряд ошибкой
  (20 с, 40 с, …); успешный ответ сбрасывает счётчик. Если здоровых не
  осталось, берётся сервер с наименьшим остатком «штрафа».
- **`upstream_policy`**: `sequential` — по порядку списка; `random` —
  случайный из здоровых.

**DNS-правила (`config dnsrule`).** Отдельные секции, привязываются к тенанту
по `id`:

| Опция | Значения | Назначение |
|---|---|---|
| `tenant` | id тенанта | Чьё правило. |
| `type` | `record` \| `forward` (в UCI при отсутствии опции — `forward`; форма LuCI по умолчанию подставляет `record`) | Тип правила. |
| `domain` | имя | `record`: точное имя — плагин отвечает готовой A-записью из `target`; `forward`: суффикс зоны — все поддомены пересылаются на серверы из `target`. |
| `target` | список | `record`: IPv4-адреса; `forward`: серверы `ip[:port]` (без порта — 53). Формы адресов те же, что у upstream. |

Домен: точная `record` имеет приоритет, затем самый узкий совпавший
`forward`-суффикс, затем обычные upstream-серверы тенанта.

```sh
config dnsrule
	option tenant 'guest'
	option type 'record'
	option domain 'printer.lan'
	list target '192.168.1.50'

config dnsrule
	option tenant 'guest'
	option type 'forward'
	option domain 'corp.example'
	list target '10.10.0.1'
```

---

## Разделы `list`

Секции `config list` — списки ресурсов: URL-подписки или ручные списки. На
них ссылаются `include_lists`/`exclude_lists` тенантов. Список может
содержать подсети и домены вперемешку — тип каждой строки определяется
автоматически.

| Опция | Тип | По умолчанию | Назначение |
|---|---|---|---|
| `name` | string | — (обязателен) | Уникальное имя списка; на него ссылаются тенанты. |
| `url` | string | пусто | URL подписки; **пусто = ручной список** (содержимое из `content`). |
| `content` | текст | — | Только для ручных списков: по одной записи в строке — подсети (`1.2.3.0/24`), адреса (`5.6.7.8`, превращаются в `/32`) и домены (`example.com`); строки `#` и `//` — комментарии; некорректные строки игнорируются. |
| `refresh` | int (часы) | `24` | Период обновления подписки; `0` — только вручную; для ручных списков не используется. |

Поведение подписок:

- при загрузке/обновлении сырой ответ сохраняется в кэш
  **`/etc/fencebreaker/lists/<имя>.txt`** и переживает рестарт;
- при ошибке загрузки список остаётся на предыдущем содержимом из кэша,
  а ошибка видна в статусе (`lists[].state = error`, `lists[].error`) и на
  странице LuCI «Списки»;
- ручные списки никогда не скачиваются и обновляются сразу при применении
  конфигурации.

```sh
config list
	option name 'blocked'
	option url 'https://example.com/blocked.txt'
	option refresh '24'

config list
	option name 'local-extra'
	option content '203.0.113.0/24
cdn.example.net
# ручная добавка'
```

---

## Примеры конфигурации

### 1. Гостевая сеть через второй WAN-интерфейс

Гости ходят в интернет через `wan2`; их DNS — через публичные резолверы,
адреса доменов продлеваются в фоне (`dns_keepalive`).

```sh
config tenant
	option id 'guest'
	option name 'Guest'
	option enabled '1'
	option egress_mode 'interface'
	option egress_interface 'wan2'
	option fallback 'main'
	option routing_mode 'select'
	# ресурсы, доступные гостям через wan2
	list include_subnets '203.0.113.0/24'
	list include_geoip 'ru'
	# клиенты: uci-сеть guest (или устройство br-guest, или CIDR)
	list networks 'guest'
	option dns_hijack '1'
	option dns_via_tunnel '1'
	option dns_keepalive '1'
	list dns_upstream '1.1.1.1'
	list dns_upstream '8.8.8.8'
	option upstream_policy 'sequential'
```

### 2. Тенант через VLESS REALITY со списком-подпиской

Подписка `streaming` пополняется внешним источником; при падении туннеля
трафик блокируется (`strict`). Нужен установленный `sing-box`.

```sh
config tenant
	option id 'corp'
	option name 'Corp'
	option enabled '1'
	option egress_mode 'vless'
	# одна строка из клиентского приложения; демон разбирает её сам
	option vless_uri 'vless://00000000-0000-0000-0000-000000000000@vpn.example.org:443?type=tcp&security=reality&sni=vpn.example.org&fp=chrome&pbk=PUBLIC_KEY&sid=SHORT_ID&flow=xtls-rprx-vision'
	option fallback 'strict'
	option routing_mode 'select'
	list include_geoip 'nl'
	list include_lists 'streaming'
	list networks 'lan'
	list dns_upstream 'https://dns.quad9.net/dns-query'

config list
	option name 'streaming'
	option url 'https://example.com/streaming.txt'
	option refresh '24'
```

### 3. Invert: всё через туннель, кроме банков и локальных сайтов

Весь трафик сети `lan` и изученные из DNS адреса идут через туннель
(`wg0`), кроме подсетей/доменов/GeoIP-исключений. Домены-исключения задаются
списком, чтобы их адреса гарантированно попали в exc-набор.

```sh
config list
	option name 'local-banks'
	option content 'bank.example
gov.example
shop.example'

config tenant
	option id 'tun-all'
	option name 'All-through-tunnel'
	option enabled '1'
	option egress_mode 'interface'
	option egress_interface 'wg0'
	option fallback 'strict'
	option routing_mode 'invert'
	# всё, кроме исключений:
	list exclude_domains 'bank.example'
	list exclude_lists 'local-banks'
	list exclude_geoip 'ru'
	list networks 'lan'
	option dns_hijack '1'
	option dns_via_tunnel '1'
	option dns_keepalive '1'
	list dns_upstream '1.1.1.1'
```

---

## Соответствие LuCI

Веб-интерфейс — меню **Сеть → fencebreaker** и **Статус → fencebreaker**.

| Страница LuCI | Что настраивает / показывает |
|---|---|
| Сеть → fencebreaker → **Тенанты** | Секции `tenant`: таблица (Название, Включён, Режим маршрутизации) и модальное окно редактирования с вкладками: **Основное** (`id` — только чтение, `priority`), **Канал** (`egress_mode`, `egress_interface`, `vless_uri`, `fallback`), **Ресурсы** (`include_subnets`, `exclude_subnets`, `include_domains`, `exclude_domains`, `include_geoip`, `exclude_geoip`, `include_lists`, `exclude_lists`), **Клиенты** (`networks`, `excluded_hosts`, `full_hosts`), **DNS** (`dns_upstream`, `upstream_policy`, `dns_hijack`, `dns_via_tunnel`, `dns_keepalive`). Ниже — сетка **DNS-правила** (`dnsrule`: `tenant`, `type`, `domain`, `target`). |
| Сеть → fencebreaker → **Списки** | Секции `list` (`name`, `url`, `refresh`, `content`) и таблица состояния списков (состояние, время обновления, число подсетей/доменов, ошибка). |
| Статус → fencebreaker → **Статус** | Файл `/var/run/fencebreaker/status.json`: демон (uptime, генерация, последняя ошибка), по тенантам — канал и его состояние (`up`/`down`/`strict`), размеры наборов, обучено DNS-адресов, счётчики и ошибки. |
| Статус → fencebreaker → **Журнал** | Системный журнал, отфильтрованный по `fencebreaker`, плюс последняя ошибка демона. |

Раздел `global` и легаси-поля `vless_*` в LuCI не редактируются — только
через UCI.

---

## Диагностика конфигурации

Проверить, что конфигурация применилась:

```sh
# журнал демона
logread | grep fencebreakerd

# агрегированный статус демона (тенанты, каналы, наборы, списки, ошибки)
cat /var/run/fencebreaker/status.json

# счётчики DNS-плагина: запросы, обучение, здоровье upstream-серверов
cat /var/run/fencebreaker/plugin_status.json

# перехваченный DNS действительно слушается?
netstat -lnup | grep 1053
```

В `status.json` обращайте внимание на:

- `daemon.last_error` — последняя ошибка применения конфигурации (та же
  строка есть в журнале и на странице LuCI «Статус»);
- `tenants[].egress.state` — `up` / `down`; `strict` означает активный
  строгий fallback (`fallback=strict` при недоступном канале);
- `tenants[].sets` — размеры nft-наборов (`inc_dns` растёт по мере обучения);
- `lists[].state` — `ok`/`error`/`pending` по каждому списку.

Типовые ошибки валидации (демон печатает их в журнал и в `last_error`;
конфигурация с ошибкой не применяется):

| Ошибка | Причина |
|---|---|
| `too many tenants` | Больше 32 секций `tenant`. |
| `bad id` / `duplicate id` | `id` не подходит под `[a-z0-9][a-z0-9_-]*` или повторяется. |
| `egress_interface is required` | Включённый тенант в режиме `interface` без `egress_interface`. |
| `vless uuid/server/port are required` | Режим `vless`, `vless_uri` пуст, а легаси-поля заполнены не полностью. |
| `reality requires public key (vless_pbk)` | `security=reality` без `pbk` (в URI — без `pbk=`). |
| `tls requires sni` | `security=tls` без `sni`. |
| `unknown vless_security` / `unknown security` | Недопустимое значение security (только `reality`, `tls`, `none`). |
| `vless_uri: transport ... not supported` | В `vless_uri` параметр `type` отличный от `tcp`. |
| `bad egress_mode` / `bad fallback` / `bad routing_mode` | Значение вне допустимых (`interface\|vless`, `main\|strict`, `select\|invert`). |
| `bad subnet "…"` | Запись в `include_subnets`/`exclude_subnets` не CIDRv4 (нужен `/маска`). |
| `bad host "…"` | `excluded_hosts`/`full_hosts` должны быть IP-адресами. |
| `at least one network is required` | У тенанта пустой `networks`. |
| `dns_upstream "…": …` | Синтаксис адреса: пустой адрес, неподдерживаемая схема, порт вне 1–65535, IPv6 с портом без скобок (`[v6]:порт`), некорректное имя хоста, невалидный `sdns://`-стамп. |
| `bad upstream_policy` | Только `sequential` или `random`. |
| `dnsrule requires domain and target` | Пустые `domain`/`target` у правила. |
| `bad dnsrule type` | Только `record` или `forward`. |
| `network "…" is claimed by tenants …` | Одна сеть указана у двух включённых тенантов — пересечения недопустимы. |
| `list: list sections require a name` | Секция `list` без `name`. |

Структурные проверки (id, дубликаты, число тенантов) применяются ко всем
секциям, требования к каналам — только к включённым тенантам.
