# fbtenant — CoreDNS-плагин fencebreaker

Внешний плагин CoreDNS для multi-tenant policy routing fencebreaker
(docs/CONTRACTS.md §2–§4). Обслуживает всех тенантов в одном процессе CoreDNS:
определяет тенанта по source IP клиента, отвечает из точных records / forwards /
upstream-серверов тенанта, обучает nftables-наборы `*_inc_dns` адресами из
A-ответов (с зеркалом состояния и без повторных добавлений) и пишет
`plugin_status.json` для демона.

## Возможности

- **Тенант по клиенту**: точное попадание в `hosts_full` приоритетнее, иначе
  longest-prefix-match по `src_nets`. Неизвестный клиент — резолв через
  `default_upstreams` (без обучения, без mark, без кэша).
- **Домен**: точная запись из `records` (A-ответ сразу) → самый узкий суффикс
  из `forwards` (trie; поддомены наследуют более широкий forward) →
  upstream-серверы тенанта.
- **Upstream-клиент** на miekg/dns: UDP, при `TC=1` — повтор по TCP, ретраи по
  списку серверов, таймаут попытки 2 с. При `dns_via_tunnel=true` исходящие
  запросы помечаются `SO_MARK=mark` тенанта (dialer.Control через
  golang.org/x/sys/unix; если выставить нельзя, например нет CAP_NET_ADMIN, —
  лог и продолжение без mark).
- **Обучение** (только A-записи положительных ответов; AAAA резолвится и
  отдаётся, но не обучается; пустые ответы/NXDOMAIN — просто возврат):
  - `learning_mode: select` — только суффиксы из `include_domains`;
  - `learning_mode: invert` — всё, кроме суффиксов `exclude_domains`.
- **Зеркало наборов** (CONTRACTS §3): IP → deadline на каждый
  `t{N}_inc_dns`/`t{N}_exc_dns`. Присутствующий и не истёкший адрес не
  добавляется повторно (счётчик `duplicates_skipped`, nft-команда не
  выполняется); при `refresh_on_remain_pct > 0` и остатке времени меньше
  процента от timeout — одно обновление; новые адреса — батчем (один вызов
  AddElements на DNS-ответ); TTL = clamp(qttl, ttl_min_sec, ttl_max_sec),
  qttl<=0 → ttl_min_sec; при переполнении `max_set_size` вытесняются самые
  старые по deadline.
- **Ресинк**: дамп наборов через `nft -j list set`, перестройка зеркал — при
  старте, при смене dns_state.json (опрос mtime/size раз в 2 с; смена
  `generation` делает ресинк обязательным), по `resync_interval_sec` и после
  ошибок nft (бэкофф до 60 с). Истёкшие из дампа уходят, неизвестные
  подхватываются.
- **Кэш** пер-тенантный (ключ tenantID+qname+qtype), лимит 10000 записей,
  вытеснение по времени, только положительные ответы.
- **plugin_status.json**: счётчики per tenant (queries, cache_hits, learned,
  duplicates_skipped, upstream_errors, set_sizes, last_error), generation,
  last_resync, resyncs. Атомарная запись (tmp+rename), раз в 5 с и при
  ресинке.

## Corefile

```
.:1053 {
    fbtenant {
        state /var/run/fencebreaker/dns_state.json
    }
    errors
}
```

`state` (обязателен) — путь к `dns_state.json`, который атомарно пишет демон
fencebreaker. Рядом с ним плагин пишет `plugin_status.json`.

## Сборка CoreDNS с плагином

Вариант 1 — из исходников coredns с plugin.cfg:

```
git clone https://github.com/coredns/coredns && cd coredns
echo 'fbtenant:github.com/leidruid/fencebreaker/coredns-plugin/fbtenant' >> plugin.cfg
go get github.com/leidruid/fencebreaker/coredns-plugin/fbtenant@latest
# для локальной разработки вместо go get:
#   go mod edit -require=github.com/leidruid/fencebreaker/coredns-plugin/fbtenant@v0.0.0
#   go mod edit -replace=github.com/leidruid/fencebreaker/coredns-plugin/fbtenant=/path/to/fencebreaker/coredns-plugin/fbtenant
go generate      # перегенерирует core/plugin/zplugin.go и дополнит go.mod
go mod tidy
go build         # coredns с fbtenant
```

Вариант 2 — отдельный main-модуль:

```
mkdir coredns-fb && cd coredns-fb
cat > main.go <<'EOF'
package main

import (
	_ "github.com/coredns/coredns/core/plugin" // встроенные плагины
	_ "github.com/leidruid/fencebreaker/coredns-plugin/fbtenant"       // fbtenant

	"github.com/coredns/coredns/coremain"
)

func main() { coremain.Run() }
EOF
cat > go.mod <<'EOF'
module coredns-fb

go 1.24

require (
	github.com/coredns/coredns v1.15.0
	github.com/leidruid/fencebreaker/coredns-plugin/fbtenant v0.0.0
)

replace github.com/leidruid/fencebreaker/coredns-plugin/fbtenant => /path/to/fencebreaker/coredns-plugin/fbtenant
EOF
go mod tidy
go build
```

Строка для plugin.cfg: `fbtenant:github.com/leidruid/fencebreaker/coredns-plugin/fbtenant`.

## Тесты

```
cd coredns-plugin/fbtenant
go build ./... && go vet ./... && go test ./...
```

Покрыто: выбор тенанта (hosts_full/LPM), trie forwards, learning-фильтр
select/invert, зеркало на моках NFTOps (дубли без nft-вызова, истёкшие,
refresh_on_remain_pct 0/50, батч, переполнение/вытеснение, ресинк), ресинк по
смене generation и по таймеру, upstream-резолвер (UDP, TCP-fallback по TC,
retry по списку серверов) на локальных miekg/dns-серверах, ServeDNS через
dnstest.NewRecorder, запись plugin_status.json, парсинг Corefile.

## Внешние зависимости

Только `github.com/coredns/coredns` (+ `github.com/coredns/caddy`), `github.com/miekg/dns`
и `golang.org/x/sys` (SO_MARK, приходит транзитивно с miekg/dns). Взаимодействие
с nftables — через бинарник `nft` (`-j list set`, `add element`, `delete
element`), fsnotify не используется.
