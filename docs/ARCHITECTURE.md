# Архитектура fencebreaker

Документ описывает, как продукт устроен внутри: компоненты, конвейер
применения конфигурации, путь DNS-запроса и пакета, жизненный цикл
изученных адресов и поведение при отказах. Пользовательская настройка —
в [CONFIGURATION.md](CONFIGURATION.md); внутренние контракты state-файлов —
в [CONTRACTS.md](CONTRACTS.md).

Схемы (исходники PlantUML — в `docs/diagrams/`):

| Схема | Файл |
|---|---|
| Компоненты | [architecture.png](diagrams/architecture.png) |
| Обучение DNS и keep-alive | [dns-learning.png](diagrams/dns-learning.png) |
| Путь пакета через nftables | [packet-flow.png](diagrams/packet-flow.png) |
| Жизненный цикл DNS-адреса | [element-lifecycle.png](diagrams/element-lifecycle.png) |

## 1. Компоненты

```
LuCI (luci-app-fencebreaker) ──► UCI /etc/config/fencebreaker
                                      │
                        fencebreakerd (демон, Go)
                 ┌──────────┬─────────┼──────────┬─────────────┐
                 ▼          ▼         ▼          ▼             ▼
           nftables    fb-coredns  sing-box   geoip-db    status.json
           (fw4+своя   (CoreDNS +  (VLESS-     (загрузка   (для LuCI)
            таблица)    fbtenant)   тенанты)    GeoIP)
```

- **`fencebreakerd`** — единственный владелец конфигурации. Читает UCI,
  валидирует, генерирует nftables-правила, конфиги CoreDNS и sing-box,
  применяет их атомарно и следит за состоянием. Запуск: procd,
  `/etc/init.d/fencebreaker`; применение повторяется при изменении UCI
  (перезапуск или SIGHUP после Save&Apply в LuCI).
- **`fb-coredns`** — один процесс CoreDNS 1.15 с внешним плагином
  `fbtenant` на все тенанты. Отвечает на DNS клиентов, обучает nft-наборы
  адресами, продлевает их (keep-alive). Никаких других DNS-функций роутера
  не затрагивает: dnsmasq продолжает обслуживать всех, кто не входит в
  тенанты.
- **`sing-box`** — опционально, по одному процессу на VLESS-тенант
  (init-скрипт `fencebreaker-singbox`). Демона нет — fencebreakerd пишет
  `config.json` и управляет процессом.
- **LuCI** — тонкий клиент: формы поверх UCI + чтение `status.json`.

## 2. Конвейер применения конфигурации

1. **UCI → модель** (`daemon/internal/model`): парсинг секций `global`,
   `tenant`, `dnsrule`, `list`; нормализация (CIDR, домены, geoip-коды),
   валидация (`Validate()`), назначение тенантам индексов 1..N.
2. **Подготовка**: резолв `networks` (имена UCI-сетей/устройства → подсети),
   загрузка списков-подписок (кэш в `/etc/fencebreaker/lists/`), сборка
   geoip-зон из базы.
3. **Применение** (`daemon/internal/apply`):
   - дамп живых DNS-элементов из nft-наборов → они переносятся в новый
     правиласт (маршрутизация «не мигает» при re-apply);
   - `nft -f` — атомарная замена таблицы `inet fencebreaker`;
   - таблицы маршрутов `t{N}` (номер `100+index`) и правила fwmark →
     таблица;
   - перегенерация `Corefile` и `/var/run/fencebreaker/dns_state.json`
     для плагина (программный интерфейс между демоном и плагином —
     см. [CONTRACTS.md](CONTRACTS.md));
   - конфиги и запуск/остановка sing-box для VLESS-тенантов.
4. **Наблюдение**: демон опрашивает состояние каналов (egress up/down),
   плагин пишет `plugin_status.json`; объединённый результат —
   `/var/run/fencebreaker/status.json` (≤10 с и по событиям), его читает
   LuCI.

Runtime-файлы (все — в tmpfs, `/var/run/fencebreaker/`):

| Файл | Автор | Назначение |
|---|---|---|
| `Corefile`, `dns_state.json` | демон | конфигурация CoreDNS/плагина |
| `applied.json` | демон | что применено (для идемпотентности) |
| `plugin_status.json` | плагин | счётчики, размеры наборов, апстримы, keeper |
| `keeper.bin` | плагин | бинарный реестр доменов keep-alive |
| `status.json` | демон | объединённый статус для LuCI |

## 3. DNS: перехват, резолв, обучение

**Перехват.** Цепочка `dns_hijack` (nat, prerouting −150, до fw4 dstnat):
для источников из `t{N}_src_nets` UDP/TCP :53 → `redirect` на порт CoreDNS
(по умолчанию 1053). Хосты из `t{N}_hosts_exc` не перехватываются (их DNS
идёт как обычно). Отключается на тенанте (`dns_hijack 0`) — тогда
DNS-обучение по доменам для него не работает.

**Резолв (плагин fbtenant).** По IP клиента определяется тенант (точное
совпадение `hosts_full`, иначе самый узкий `src_nets`). Дальше:

- статическая запись (`dnsrule type=record`) отвечает сразу;
- кэш ответов тенанта (по TTL ответа);
- выбор апстрима: узкая forward-зона (`dnsrule type=forward`) → список
  `dns_upstream`; порядок `sequential`/`random` с учётом здоровья:
  недоступный апстрим штрафуется на 10 с, штраф удваивается с каждой
  ошибкой подряд, успех сбрасывает; запрос идёт с `SO_MARK` fwmark'а
  тенанта при `dns_via_tunnel=1` (upstream-DNS сам идёт через канал);
- поддерживаются 11 форм адресов: UDP/TCP (v4/v6, с портом),
  `udp://`, `tcp://`, `tls://`, `https://` (DoH), `h3://` (только
  HTTP/3, без фолбэка), `quic://` (DoQ), `sdns://` (стампы DNSCrypt/DoH).

**Обучение.** A-записи успешных ответов заносятся в nft-наборы с
per-element `timeout = clamp(TTL, ttl_min..ttl_max)` (по умолчанию
300…86400 с):

- домен входит в `include_domains` → `t{N}_inc_dns`;
- домен входит в `exclude_domains` → `t{N}_exc_dns`;
- в режиме `select` прочие домены не изучаются; в `invert` изучается всё
  (кроме исключений) → `inc_dns`;
- живой элемент повторно не добавляется (зеркало в памяти плагина держит
  deadline каждого IP); `dns_refresh_on_remain_pct > 0` включает
  пассивное продление при повторных запросах клиента;
- зеркало сверяется с ядром ресинком (раз в `resync_interval`, по
  изменению `generation`, после ошибок nft).

**Keep-alive (`dns_keepalive`).** Обучение пассивно: элемент живёт ровно
clamp(TTL) с первого изучения, а правила маркировки матчат каждый пакет —
по истечении маршрутизация тенанта пропадает до следующего DNS-запроса
какого-нибудь клиента. Keeper ведёт реестр доменов тенанта и за
`max(60 с, 25%·TTL)` до истечения фоново перезапрашивает домен (скан раз
в 15 с, до 8 параллельно, тот же выбор апстрима/mark) и безусловно
продлевает элементы. NXDOMAIN/пустой ответ удаляют домен из реестра,
3 ошибки подряд — возврат к пассивному режиму. Реестр сохраняется в
`keeper.bin` (бинарный, атомарная запись) и восстанавливается при
рестарте сервиса — продление продолжается без клиентских запросов.

## 4. nftables и маршрутизация

Таблица `inet fencebreaker` (не зависит от fw4, но уживается с ним):

```
chain dns_hijack  (nat prerouting -150)
    ip saddr @t1_hosts_exc  udp/tcp dport 53 return
    ip saddr @t1_src_nets   udp/tcp dport 53 redirect to :1053

chain mangle_pre  (filter prerouting -150)
    ip saddr @t1_src_nets  jump t1_fwd
    ip saddr @t1_hosts_full jump t1_fwd

chain t1_fwd      (порядок правил = приоритет)
    ip saddr @t1_hosts_exc  return                 # хост «мимо тенанта»
    ip saddr @t1_hosts_full meta mark set 0x1000 return   # хост «всегда через канал»
    ip daddr @t1_exc_geoip  return                 # исключения…
    ip daddr @t1_exc_static return
    ip daddr @t1_exc_dns    return
    ip daddr @t1_inc_geoip  meta mark set 0x1000 return   # включения…
    ip daddr @t1_inc_static meta mark set 0x1000 return
    ip daddr @t1_inc_dns    meta mark set 0x1000 return
    meta mark set 0x1000 return                    # только в режиме invert
```

- `*_static` — подсети из конфига (interval-наборы), `*_geoip` — зоны
  стран, `*_dns` — изученные адреса (timeout-наборы, см. выше).
- Маркировка в **prerouting** — обязательна: в forward решение о
  маршруте уже принято.
- fwmark `0x1000 + (index−1)` → правило `ip rule` → таблица `100+index`;
  в таблице — default через канал тенанта.

**Каналы:**

- `interface` — таблица тенанта получает default через указанный интерфейс
  (`egress_interface`; шлюз берётся из default основной таблицы, для
  p2p-интерфейсов — device-маршрут). Канал «упал» (нет default/интерфейса)
  — статус egress down; поведение по `fallback`:
  - `main` — таблица тенанта просто не имеет default, пакеты падают в
    основную таблицу (обычная маршрутизация);
  - `strict` — в таблицу ставится `unreachable`-default: пакеты тенанта
    отбрасываются, утечки мимо канала нет.
- `vless` — fencebreakerd поднимает sing-box (один процесс, по TUN-интерфейсу
  `fb-tun{N}` на тенант, адрес `10.231.N.1/30`, `auto_route=false`):
  демона нет — таблица тенанта получает device-default через TUN. Исходящие
  соединения sing-box к VLESS-серверу привязываются к обычному интерфейсу
  (`auto_detect_interface`), замыкания на самого себя не возникает.
  Параметры — из строки `vless://…` (REALITY/TLS) или отдельных опций.
- При применении конфигурации демон сбрасывает conntrack-записи с fwmark
  тенантов (иначе давно живые соединения продолжали бы идти по старым
  маршрутам) и ослабляет `rp_filter` на интерфейсных каналах.

## 5. Отказоустойчивость и обновления

- procd перезапускает упавшие процессы (respawn).
- Re-apply демона переносит живые DNS-элементы в новый правиласт
  (dump → restore), поэтому изменение конфигурации не обрывает
  маршрутизацию по уже изученным адресам.
- Рестарт `fb-coredns`: наборы живут в ядре, плагин перестраивает
  зеркало дампами; реестр keep-alive — из `keeper.bin`.
- Обновление пакетов: postinst перезапускает работающие сервисы
  (демона и fb-coredns), конфигурация `/etc/config/fencebreaker` и кэш
  списков сохраняются.

## 6. Изоляция тенантов

- У каждого тенанта свои наборы, fwmark, таблица маршрутов и (для VLESS)
  TUN+процесс — пересечений нет; DNS-запросы маршрутизируются по
  источнику, ответы не смешиваются (кэш и апстримы — на тенанта).
- `priority` (меньше — важнее) решает конфликты, когда хост/сеть
  попадают в несколько тенантов (`hosts_full`).
- Хосты `excluded_hosts` полностью вне тенанта: ни DNS-перехвата, ни
  маркировки.
