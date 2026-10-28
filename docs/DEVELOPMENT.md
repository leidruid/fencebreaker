# Разработка и сборка fencebreaker

Документ для контрибьюторов: структура репозитория, локальная сборка
пакетов, тесты, соглашения. Установка на роутер — [INSTALL.md](INSTALL.md),
настройка — [CONFIGURATION.md](CONFIGURATION.md), обзор —
[README.md](../README.md).

## Структура репозитория

```
daemon/                     демон fencebreakerd (Go-модуль github.com/leidruid/fencebreaker/daemon)
coredns-plugin/fbtenant/    плагин fbtenant для CoreDNS (отдельный Go-модуль)
net/fencebreaker/           пакет OpenWrt: демон; init-скрипты и uci-defaults в files/
net/fencebreaker-coredns/   пакет CoreDNS с плагином: patches/, files/go.sum.add
luci/luci-app-fencebreaker/ LuCI-приложение (views, po/ru, ACL, menu.d)
scripts/                    install-openwrt.sh, local-build-sdk.sh, make-apk-repo.sh
tests/e2e/                  e2e-смоуки: local_smoke.sh, dns_smoke.sh
docs/                       CONTRACTS.md, PLAN.md, diagrams/
.github/workflows/ci.yml    CI
out/                        артефакты локальной сборки (создаётся, не в git)
```

Пакеты: `fencebreaker` (демон, версии 0.1.0), `fencebreaker-coredns`
(CoreDNS 1.15.0 с плагином), `luci-app-fencebreaker` (архитектура `all`,
зависимости `+fencebreaker +fencebreaker-coredns`; i18n-пакет
`luci-i18n-fencebreaker-ru` собирается из `po/ru` автоматически через
`luci.mk`).

## Сборка из исходников

Требуется **docker**. Скрипт собирает пакеты в официальном SDK-образе:

```sh
./scripts/local-build-sdk.sh                  # все три пакета
./scripts/local-build-sdk.sh fencebreaker     # один пакет
./scripts/local-build-sdk.sh fencebreaker-coredns luci-app-fencebreaker
```

Механика: репозиторий монтируется в контейнер как `/fb` и подключается к SDK
как `src-link`-фид `fencebreaker` (`feeds.conf` обязательно сохраняет записи
`packages`/`luci` из `feeds.conf.default` — только наша строка отключила бы
штатные фиды); `setup.sh` образа скачивает и распаковывает tarball SDK;
затем `feeds update/install -a`, `CONFIG_PACKAGE_<pkg>=m`,
`make package/<pkg>/compile`. Готовые `.apk` и индексы копируются в
`out/apk/` (`bin/packages/x86_64/fencebreaker/*.apk` и `APKINDEX.tar.gz`);
в конце контейнера запускается `make-apk-repo.sh /builder/bin/packages`
(при его неудаче копируются только raw-файлы с предупреждением).

Переменные окружения:

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `SDK_IMAGE` | `openwrt/sdk:x86-64-openwrt-25.12` | docker-образ SDK |
| `SDK_VERSION_PATH` | `releases/25.12.0` | путь версии SDK для `setup.sh` |
| `SDK_TARGET` | `x86/64` | цель SDK |
| `SDK_CACHE` | `1` | `0` — отключить docker-том |

По умолчанию создаётся именованный docker-том `fencebreaker-sdk-builder`,
в котором персистится `/builder`: SDK и toolchain не скачиваются повторно,
что заметно ускоряет повторные сборки. `SDK_CACHE=0` собирает без тома
(чистый прогон).

### Установка своих сборок на роутер

На OpenWrt по умолчанию нет sftp-server, а новые версии `scp` по умолчанию
работают по SFTP — нужен флаг `-O`:

```sh
scp -O scripts/install-openwrt.sh root@ROUTER:/tmp/
scp -O -r out/apk/bin/packages/x86_64/fencebreaker root@ROUTER:/tmp/fb-apk
ssh root@ROUTER 'sh /tmp/install-openwrt.sh --dir /tmp/fb-apk'
```

Режим `--dir` ставит новейшую версию каждого пакета из каталога и всегда
использует `--allow-untrusted` (локальные `.apk` не подписаны). Подробности
режимов установщика — [INSTALL.md](INSTALL.md).

### Свой apk-репозиторий

`scripts/make-apk-repo.sh <каталог с *.apk>` создаёт `APKINDEX.tar.gz` в
каждом каталоге, содержащем `.apk` (apk-репозиторий — один индекс на
каталог). Бинарник `apk` ищется в `PATH`, затем в `./staging_dir` (запуск
из корня распакованного SDK), либо явно через `APK_BIN=<путь>` (обычно
`staging_dir/host/bin` или `staging_dir/hostpkg/bin`). На apk-tools 3
(OpenWrt 25.12) индекс строится `apk mkndx`, для старых toolchain есть
fallback на v2 `apk index`.

Подпись индекса: `APK_SIGN_KEY=<путь к приватному PEM>` — `mkndx` встраивает
подпись в индекс (`--sign-key`), fallback — `openssl dgst -sha256 -sign`
(файл `APKINDEX.tar.gz.sig`). Публичный ключ ставится на роутер в
`/etc/apk/keys/` (см. «Установка из apk-репозитория» в
[README.md](../README.md)). Внимание: сценарий подписи ключом пока не
проверялся реальной установкой (см. «Статус реализации» в README).

## Тесты

Go-кода два **независимых модуля** (go.work не используется): `daemon/` и
`coredns-plugin/fbtenant/`.

```sh
(cd daemon && go build ./... && go vet ./... && go test ./... -count=1)
(cd coredns-plugin/fbtenant && go build ./... && go vet ./... && go test ./... -count=1)
```

e2e-смоуки (подробности — в шапках скриптов):

```sh
# демону нужен бинарник:
(cd daemon && go build -o fencebreakerd ./cmd/fencebreakerd)
tests/e2e/local_smoke.sh [WORKDIR]
#   реальный nftables/netlink: требует root (скрипт сам перезапускается через
#   sudo) и nft/ip; создаёт и удаляет таблицу inet fencebreaker и ip rule
#   в диапазоне приоритетов 11000–12000; WORKDIR по умолчанию /tmp/fencebreaker-smoke

tests/e2e/dns_smoke.sh [COREDNS_BIN] [WORKDIR]
#   плагин fbtenant: нужен coredns, собранный С fbtenant (по умолчанию
#   /tmp/fencebreaker-dns-smoke/coredns-fb) и dig; проверяет обучение inc_dns, попадание
#   исключений в exc_dns, отсутствие повторного добавления живых элементов
#   и счётчики plugin_status.json; в CI не запускается
```

Shell-скрипты проверяются `shellcheck scripts/*.sh` (в CI шаг пропускается,
если shellcheck не установлен).

## Как устроены пакеты с Go-кодом

**`net/fencebreaker` (демон).** Источники лежат в самом репозитории, поэтому
у пакета нет `PKG_SOURCE`: `Build/Prepare` копирует `daemon/go.mod`,
`go.sum`, `cmd/` и `internal/` в build-dir. Собирается
`cmd/fencebreakerd`; версия прошивается линкером
(`GO_PKG_LDFLAGS_X ...Version=$(PKG_VERSION)`). Пакет ставит
`/usr/sbin/fencebreakerd`, три init-скрипта (`fencebreaker`,
`fencebreaker-coredns`, `fencebreaker-singbox`), uci-defaults
(`99-fencebreaker`: создаёт `/etc/config/fencebreaker` из шаблона
`/usr/share/fencebreaker/fencebreaker.conf` при отсутствии и секцию
`ucitrack`) и пустой `/etc/fencebreaker/lists`. Зависимости: `nftables`,
`kmod-nft-nat`, `ca-bundle`, `ca-certificates`, `fencebreaker-coredns`.
`sing-box` зависимостью **не** является (нужен только для VLESS-тенантов,
его init-скрипт не стартует при отсутствии бинарника). postinst
перезапускает работающего демона после обновления пакета.

**`net/fencebreaker-coredns` (CoreDNS с плагином).** Собирается из
официального tarball CoreDNS v1.15.0, механика включения плагина:

1. стандартный патч-сет `patches/001-add-fbtenant-plugin.patch` вписывает
   плагин в `plugin.cfg` и генерируемые `zplugin`/`zdirectives`;
2. `Build/Prepare` копирует `coredns-plugin/fbtenant` в `plugin/fbtenant`
   и дописывает в `go.mod` строки `require`/`replace`: сам плагин
   (`replace github.com/leidruid/fencebreaker/coredns-plugin/fbtenant => ./plugin/fbtenant`) и его
   зависимости (`ameshkov/dnscrypt`, `ameshkov/dnsstamps`,
   `aead/poly1305`, `aead/chacha20`);
3. сборка идёт с замороженным `go.sum` CoreDNS, поэтому хэши модулей,
   добавленных плагином, досыпаются из `files/go.sum.add`
   (`cat files/go.sum.add >> go.sum`).

Следствие: **при добавлении новой go-зависимости в плагин** нужно обновить
`net/fencebreaker-coredns/files/go.sum.add` — прогоните
`(cd coredns-plugin/fbtenant && go mod tidy)` и перенесите новые строки
`go.sum` плагина в этот файл. Бинарник ставится как `/usr/bin/fb-coredns`,
чтобы не конфликтовать с другими пакетами coredns; postinst перезапускает
`fencebreaker-coredns`, если уже существует `/var/run/fencebreaker/Corefile`.

## CI

`.github/workflows/ci.yml` — три job'а:

- **test** — `go build/vet/test` обоих модулей (go `stable`; кэш модулей
  отключён, поскольку модулей два) плюс shellcheck скриптов;
- **build-sdk** — контейнер `openwrt/sdk:x86-64-openwrt-25.12`
  (`--user root`): `setup.sh` скачивает SDK (`TARGET=x86/64`,
  `VERSION_PATH=releases/25.12.0`), репозиторий подключается `src-link`
  фидом, собираются три пакета, индекс строит `make-apk-repo.sh`; артефакт
  `apk-packages` (все `*.apk` и `APKINDEX.tar.gz`);
- **release** — только на тегах `v*` при успешных `test` и `build-sdk`:
  выкладывает собранные `*.apk` и `APKINDEX.tar.gz` как ассеты GitHub
  Release соответствующего тега. Установщик по умолчанию ставит пакеты
  именно из Releases (последний релиз или `--version TAG`). На обычные
  push/PR выкладываются только артефакты сборки.

## Соглашения

- **[CONTRACTS.md](CONTRACTS.md) — источник внутренних контрактов**: пути
  (`/etc/config/fencebreaker`, `/etc/fencebreaker/lists/`,
  `/var/run/fencebreaker/`), схема UCI (§1), формат `dns_state.json`
  демон→плагин (§2), синхронизация плагина с nftables (§3),
  `plugin_status.json` (§4), `status.json` (§5), взаимодействие процессов
  (§6).
- Любое изменение интерфейсов демон↔плагин (`dns_state.json`,
  `plugin_status.json`, имена/поколения nft-наборов, UCI-опции) обязано
  сопровождаться обновлением CONTRACTS.md в том же изменении.
- Язык: LuCI-строки (`po/ru`) и документация — на русском.
