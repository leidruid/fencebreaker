# fencebreaker — контракты между компонентами

Версия контрактов: 1. Общие соглашения:

- Имя проекта: `fencebreaker`. Службы procd: `fencebreaker` (демон),
  `fencebreaker-coredns` (единственный CoreDNS-процесс), `fencebreaker-singbox`
  (sing-box, только если есть VLESS-тенанты).
- Каталоги: конфиг `/etc/config/fencebreaker`; данные списков
  `/etc/fencebreaker/lists/`; runtime-состояние `/var/run/fencebreaker/`.
- Маршрутизационные таблицы: `100 + N` (N — порядковый индекс тенанта, 1-based,
  стабилен пока тенант существует). fwmark тенанта: точное значение
  `0x1000 + (N-1)` (диапазон 0x1000–0x101F; ip rule сравнивает точное
  значение, без маски). Макс. 32 тенанта — валидируется.
- Таблица nftables: `inet fencebreaker`. Наборы на тенанта `t{N}`:
  `t{N}_inc_static`, `t{N}_inc_geoip` (flags interval), `t{N}_inc_dns`
  (flags timeout), `t{N}_exc_static`, `t{N}_exc_geoip` (interval),
  `t{N}_exc_dns` (timeout), `t{N}_src_nets` (interval), `t{N}_hosts_exc`,
  `t{N}_hosts_full`; глобальные агрегаты `all_src_nets` (interval),
  `all_hosts_exc`. Цепочки: `dns_hijack` (nat prerouting, priority -200),
  `mangle_fwd` (filter hook forward, priority -150), на тенанта — `t{N}_fwd`
  (jump из mangle_fwd при `ip saddr @t{N}_src_nets` или `ip saddr @t{N}_hosts_full`).
- IPv4 в v1; имена и структура выбраны совместимо с добавлением IPv6-наборов
  (`..._v6`) позже.

## 1. UCI: /etc/config/fencebreaker

```
config fencebreaker 'global'
    option enabled '1'
    option geodata_url 'https://.../geoip.dat'
    option geodata_refresh '168'      # часы; 0 — только вручную
    option lists_refresh '24'         # часы
    option coredns_port '1053'
    option ttl_min '300'
    option ttl_max '86400'
    option dns_refresh_on_remain_pct '0'   # 0 = не продлевать timeout повторами
    option max_set_size '65536'
    option resync_interval '60'       # секунды, ресинк зеркал плагина

config tenant
    option id 'tenant1'               # уникальный id (a-z0-9_), генерируется
    option name 'Guest'
    option enabled '1'
    option egress_mode 'interface'    # interface | vless
    option egress_interface 'eth1'    # имя УСТРОЙСТВА (netlink LinkByName, не uci-сеть); LuCI грузит список из `ubus call network.device status`
    # канал vless (egress_mode=vless) — рекомендуемый способ, одна строка:
    option vless_uri 'vless://uuid@example.org:443?type=tcp&security=reality&sni=example.org&fp=chrome&pbk=...&sid=...&flow=xtls-rprx-vision#name'
    # (демон парсит её сам; transport — только tcp; security по умолчанию none;
    #  fp по умолчанию chrome; фрагмент #имя игнорируется)
    # Легаси-поля (используются, только если vless_uri пуст):
    option vless_uuid '...'
    option vless_server 'example.org'
    option vless_port '443'
    option vless_flow 'xtls-rprx-vision'
    option vless_security 'reality'   # reality | tls | none
    option vless_sni 'example.org'
    option vless_fp 'chrome'
    option vless_pbk '...'            # reality public key
    option vless_sid '...'            # reality short-id
    option fallback 'main'            # main | strict
    option routing_mode 'select'      # select | invert
    list include_subnets '1.2.3.0/24'
    list include_domains 'example.com'
    list include_geoip 'ru'
    list include_lists 'blocked'
    list exclude_subnets '5.6.7.8/32'
    list exclude_domains 'bank.example'
    list exclude_geoip 'cn'
    list exclude_lists 'trusted'
    list networks 'guest'             # uci-сети и/или имена устройств (br-guest)
    list excluded_hosts '192.168.1.50'
    list full_hosts '192.168.1.60'
    option dns_hijack '1'
    option dns_via_tunnel '1'
    option dns_keepalive '0'           # продление адресов доменов до истечения TTL
    list dns_upstream '1.1.1.1'
    option priority '100'             # меньшее число — важнее (hosts_full конфликтов)

config dnsrule
    option tenant 'tenant1'
    option type 'record'              # record | forward
    option domain 'api.corp'          # record: точный A; forward: суффикс зоны
    list target '10.0.0.5'            # record: IP; forward: 'ip[:port]'

config list
    option name 'blocked'
    option url 'https://.../list.txt' # пусто => ручной список (content)
    option content ''                 # ручной список: по одной записи в строке
                                      # (#-комментарии допустимы; парсится как подписка)
    option refresh '24'               # часы; 0 — только вручную; для ручных не используется
```

## 2. dns_state.json (демон → плагин fbtenant)

Путь: `/var/run/fencebreaker/dns_state.json`. Демон пишет атомарно
(tmp+rename). `generation` увеличивается, когда демон пересоздал наборы
`*_inc_dns`/`*_exc_dns` (например, смена id/удаление тенанта); при обычном
re-apply (содержимое сохранено) generation не меняется — плагин продолжает
работу со своим зеркалом.

```json
{
  "version": 1,
  "generation": 3,
  "listen_port": 1053,
  "resync_interval_sec": 60,
  "ttl_min_sec": 300,
  "ttl_max_sec": 86400,
  "refresh_on_remain_pct": 0,
  "max_set_size": 65536,
  "default_upstreams": ["127.0.0.1:53"],
  "nft_table": "fencebreaker",
  "tenants": [
    {
      "id": "tenant1",
      "mark": 4096,
      "src_nets": ["192.168.100.0/24"],
      "hosts_full": ["192.168.1.60"],
      "sets": {
        "inc_dns": "t1_inc_dns",
        "exc_dns": "t1_exc_dns"
      },
      "learning_mode": "select",
      "include_domains": ["example.com"],
      "exclude_domains": ["bank.example"],
      "upstreams": ["1.1.1.1"], "upstream_policy": "sequential",
      "dns_via_tunnel": true,
      "dns_keepalive": false,
      "records": [ {"domain": "api.corp", "ips": ["10.0.0.5"]} ],
      "forwards": [ {"domain": "corp.example", "servers": ["10.10.0.1:53"]} ]
    }
  ]
}
```

Семантика (реализует плагин):
- Тенант по клиенту: longest-prefix по `src_nets`; точное совпадение в
  `hosts_full` приоритетнее; не найден → `default_upstreams` (dnsmasq).
- Домен: точная запись из `records` → самый узкий суффикс из `forwards`
  (поддомены наследуют) → `upstreams` тенанта.
- Обучение: `select` — домены из `include_domains` (суффиксы) → `inc_dns`;
  `invert` — все остальные → `inc_dns`. Ответы доменов из `exclude_domains`
  всегда обучаются в `exc_dns` (в invert режиме их обязаны накрывать
  исключения, а не catch-all; в select — исключение сильнее include-подсети).
  Кандидаты — A-записи.
- `dns_via_tunnel=true` → исходящие upstream-запросы с `SO_MARK=mark`.
- `learning_mode` соответствует `routing_mode` тенанта.

### 2.1. Форматы DNS-upstream (опция tenant.dns_upstream / поле dns_state tenants[].upstreams)

Любая комбинация, по одной записи в списке (адреса для примера):

| Форма | Значение |
|---|---|
| `94.140.14.140`, `2a10:50c0::1:ff` | обычный DNS, UDP :53 |
| `94.140.14.140:53`, `[2a10:50c0::1:ff]:53` | обычный DNS, UDP с портом |
| `udp://host[:port]` | UDP по имени хоста |
| `tcp://ip\|[v6][:port]` | DNS поверх TCP |
| `tcp://host` | TCP по имени хоста |
| `tls://host[:853]` | DNS-over-TLS |
| `https://host[:443][/dns-query]` | DNS-over-HTTPS |
| `h3://host[:443][/dns-query]` | DoH, только HTTP/3 (без отката к h2) |
| `quic://host[:853]` | DNS-over-QUIC (RFC 9250) |
| `sdns://…` | DNS Stamp: DNSCrypt или DoH |

Поле `upstream_policy` (UCI `option upstream_policy`): `sequential` (по порядку, по умолчанию) | `random` (случайный из здоровых).

Доступность: неответивший upstream помечается неиспользуемым на 10 с, срок удваивается с каждой следующей подряд ошибкой (20с, 40с, …); успех сбрасывает счётчик. Если здоровых нет, выбирается сервер с наименьшим остатком штрафа. Состояние — в plugin_status.json (`tenants[].upstreams[]`: addr/state/consecutive_failures/penalty_until/last_error/success/failures).

SO_MARK (dns_via_tunnel) применяется на udp/tcp/tls/https/h3/quic/DNSCrypt-сокетах; bootstrap-резолв имён хоста и получение сертификата DNSCrypt идут без mark. v1: пути sdns с пиннингом сертификата не проверяются.

## 3. Синхронизация плагина с nftables (ключевое требование)

- Плагин держит зеркало каждого набора (`inc_dns`/`exc_dns` тенантов):
  IP → deadline (время истечения элемента).
- Все операции с nftables в плагине — за изолированным интерфейсом
  (`NFTOps`), чтобы логика тестировалась на моках. Реализация по умолчанию —
  через бинарник `nft`: дамп `nft -j list set inet <table> <set>`,
  добавление `nft add element inet <table> <set> '{ a.b.c.d timeout 300s, ... }'`
  (батч на один DNS-ответ), удаление `nft delete element ...`.
- Инициализация и ресинк: дамп реального содержимого наборов: при старте,
  при замене dns_state.json (плагин опрашивает файл раз в 2 с по mtime/размеру),
  по `resync_interval_sec`, после ошибок nft (бэкофф), и обязательно, если
  изменился `generation`.
- Перед добавлением адреса — проверка зеркала: присутствующий и не истёкший
  адрес НЕ добавляется повторно (netlink-транзакция не выполняется).
- Если `refresh_on_remain_pct > 0` и остаток времени < указанного процента от
  исходного timeout — элемент обновляется (одна транзакция обновления).
- Новые адреса добавляются батчем (одна netlink-транзакция на DNS-ответ) с
  timeout = TTL, зажатым в [ttl_min_sec, ttl_max_sec].
- При превышении `max_set_size` — вытеснение самых старых элементов набора.
- Источник истины — nftables; зеркало — кэш членства.
- `dns_keepalive=true` (UCI-опция тенанта `dns_keepalive`, по умолчанию 0):
  продление адресов доменов. Обучение пассивно — элемент живёт ровно
  clamp(TTL) от первого изучения, а правила маркировки матчат каждый пакет,
  поэтому после истечения маршрутизация тенанта пропадает до следующего
  DNS-запроса клиента. Keeper ведёт реестр изученных доменов тенанта и
  фоново (скан раз в 15 с, до 8 параллельных запросов, тот же выбор
  апстримов/политика/health/SO_MARK, без учёта в client-статистике)
  перезапрашивает домен, когда до deadline осталось
  max(60 с, 25%·timeout), и безусловно продлевает элементы (Refresh).
  Результаты: успех → продление и сброс счётчика ошибок; NXDOMAIN/пустой
  ответ → запись удаляется (элемент истекает сам); 3 подряд ошибок
  апстрима → запись удаляется (возврат к пассивному режиму). IP,
  исчезнувшие из нового ответа, не удаляются (общие CDN-IP доменов) и
  истекают по своему timeout.
- Реестр keeper персистится в бинарный файл `/var/run/fencebreaker/keeper.bin`
  (tmp+rename; переживает рестарт сервиса, не ребут). Формат (LE):
  `"FBKP" | u8 version=1 | u8 flags=0 | u32 count`, затем записи
  `u16 nameLen name | u8 tenantLen tenant | u8 kind(1=inc,2=exc) | u32 timeoutSec | i64 deadlineUnixSec`.
  При загрузке записи с deadline старше 24 ч отбрасываются; повреждённый
  файл игнорируется (старт с пустым реестра, warning в лог).

## 4. plugin_status.json (плагин → демон, для status.json)

Путь: `/var/run/fencebreaker/plugin_status.json`, атомарная запись, раз в 5 с
и при ресинке:

```json
{
  "updated": "2026-10-25T12:00:00Z",
  "generation": 3,
  "last_resync": "2026-10-25T12:00:00Z",
  "resyncs": 42,
  "tenants": {
    "tenant1": {
      "queries": 1000, "cache_hits": 500, "learned": 80,
      "duplicates_skipped": 520, "upstream_errors": 2,
      "set_sizes": {"inc_dns": 80}, "last_error": "",
      "keeper": {"domains": 12, "refreshes": 40, "errors": 1,
                 "last_refresh": "2026-10-25T11:59:00Z"}
    }
  }
}
```

## 5. status.json (демон → LuCI)

Путь: `/var/run/fencebreaker/status.json`, обновление ≤ каждые 10 с и по
событиям. Объединяет состояние демона и plugin_status.json:

```json
{
  "version": 1,
  "updated": "...",
  "daemon": {"generation": 3, "uptime_sec": 120, "last_apply": "...", "last_error": ""},
  "tenants": [
    {
      "id": "tenant1", "name": "Guest", "enabled": true,
      "table": 101, "mark": 4096,
      "egress": {"mode": "interface", "target": "wan2", "state": "up"},
      "routing_mode": "select", "fallback": "main",
      "sets": {"inc_static": 10, "inc_geoip": 45230, "inc_dns": 80,
               "exc_static": 0, "exc_geoip": 0, "exc_dns": 3},
      "dns": {"queries": 1000, "learned": 80, "duplicates_skipped": 520,
              "upstream_errors": 2,
              "keeper": {"domains": 12, "refreshes": 40, "errors": 1,
                         "last_refresh": "..."}},
      "errors": []
    }
  ],
  "lists": [ {"name": "blocked", "state": "ok", "updated": "...", "nets": 900, "domains": 300, "error": ""} ],
  "geodata": {"state": "ok", "updated": "...", "entries": 61000, "error": ""}
}
```

LuCI читает его через ubus `file read` (см. ACL в luci-приложении).

## 6. Взаимодействие процессов

- Демон владеет структурой nftables (наборы/цепочки/правила) и таблицами
  маршрутизации; плагин — только элементами `*_inc_dns`/`*_exc_dns`.
- Демон применяет правила через `nft -f` (текст), маршруты/rules — netlink.
- Переконфигурация: SIGHUP демону (procd reload). Демон перечитывает UCI,
  регенерирует всё, при изменении Corefile/state — `restart` служб
  coredns/singbox только когда необходимо (конфиг синг-бокса/порта изменился),
  иначе ничего не перезапускает.
- sing-box: один процесс, конфиг `/var/run/fencebreaker/sing-box.json`,
  inbound `tun` `fb-tun{N}` на каждого VLESS-тенанта (auto_route=false),
  `route.auto_detect_interface=true`.
