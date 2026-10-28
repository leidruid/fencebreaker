# Установка fencebreaker на OpenWrt

fencebreaker — multi-tenant policy routing для OpenWrt: трафик выбранных сетей
и хостов направляется через канал тенанта (обычный интерфейс или VLESS через
sing-box), DNS обслуживает единый CoreDNS с плагином `fbtenant`, обучающий
nft-наборы адресами из ответов.

Обзор проекта — [README.md](../README.md), настройка тенантов —
[CONFIGURATION.md](CONFIGURATION.md), контракты и схема UCI —
[CONTRACTS.md](CONTRACTS.md).

## Требования

- **OpenWrt 25.12+ с менеджером пакетов apk.** opkg не поддерживается —
  скрипт установки проверяет наличие `apk`. OpenWrt 24.09 тоже использует
  apk и, вероятно, работоспособна, но не тестируется: пакеты собираются
  под 25.12.
- **Архитектура x86/64** (`x86_64`) — единственная поддерживаемая в v1.
- **fw4/nftables** — стандартный файрвол OpenWrt; ничего доустанавливать не
  нужно (пакет `fencebreaker` сам тянет зависимости `nftables` и
  `kmod-nft-nat`, а также `ca-bundle`/`ca-certificates`).
- **~60 МБ свободного места** в корневом overlay: пакет
  `fencebreaker-coredns` разворачивается в ~35 МБ плюс зависимости.
- Доступ к GitHub (для автоустановки из Releases) и к официальному
  packages-feed OpenWrt (нужен, если ставится `sing-box`).
- **sing-box — опционально**, только если у тенантов нужен канал VLESS.
  Жёсткой зависимостью он не является: init-скрипт `fencebreaker-singbox`
  не стартует sing-box при отсутствии бинарника.

Что ставится:

| Пакет | Содержимое |
|---|---|
| `fencebreaker` | демон `/usr/sbin/fencebreakerd`, init-скрипты `fencebreaker`, `fencebreaker-coredns`, `fencebreaker-singbox`, uci-defaults |
| `fencebreaker-coredns` | CoreDNS с плагином `fbtenant` как `/usr/bin/fb-coredns` (зависимость `fencebreaker`) |
| `luci-app-fencebreaker` | LuCI-приложение |
| `luci-i18n-fencebreaker-ru` | русский перевод LuCI |
| `sing-box` | только с `--with-singbox`, из официального packages-feed |

## Автоустановка

Скопируйте установщик на роутер и выполните (OpenWrt по умолчанию не содержит
sftp-server, поэтому современному `scp` нужен флаг `-O`):

```sh
scp -O scripts/install-openwrt.sh root@<адрес-роутера>:/tmp/
ssh root@<адрес-роутера> 'sh /tmp/install-openwrt.sh'
```

### Режимы установки

**Из GitHub Releases (по умолчанию).** Без опций скрипт ставит пакеты
последнего релиза проекта; `--version TAG` — конкретного (например,
`--version v0.1.0`). Пакеты скачиваются как ассеты релиза и ставятся без
проверки подписи (`--allow-untrusted` включён всегда). Нужен `curl`.

> Сборки для каждого релиза готовит CI и прикладывает `.apk` к релизу. Если
> нужной версии среди релизов нет или вы собираете пакеты сами — используйте
> режим `--dir` или apk-репозиторий через `--repo URL`.

**Из apk-репозитория (`--repo URL`).** Явный URL репозитория с подписанным
или доверенным индексом (например, собственное зеркало); подписи можно
не проверять флагом `--allow-untrusted`.

**Из локального каталога с .apk (`--dir DIR`).** Для собственных сборок —
соберите пакеты (см. [DEVELOPMENT.md](DEVELOPMENT.md)), затем:

```sh
scp -O -r out/apk/bin/packages/x86_64/fencebreaker root@<адрес-роутера>:/tmp/fb-apk
ssh root@<адрес-роутера> 'sh /tmp/install-openwrt.sh --dir /tmp/fb-apk'
```

В режиме `--dir` проверка подписи не выполняется (`--allow-untrusted`
включён всегда). Скрипт берёт новейшую версию каждого пакета в каталоге;
обязательны `fencebreaker`, `fencebreaker-coredns` и
`luci-app-fencebreaker`, пакет `luci-i18n-fencebreaker-ru` необязателен
(без него интерфейс будет английским, скрипт выведет предупреждение).

### Опции

| Опция / переменная | Действие |
|---|---|
| `--version TAG` | конкретный релиз-тег GitHub (например `v0.1.0`); по умолчанию — последний релиз |
| `--repo URL` | apk-репозиторий fencebreaker (взаимоисключим с `--version`/`--dir`) |
| `--dir DIR` | каталог с собранными `.apk` (взаимоисключим с `--version`/`--repo`) |
| `--with-singbox` | дополнительно установить `sing-box` (нужен только для VLESS-каналов); можно доустановить позже тем же флагом |
| `--no-i18n` | не ставить `luci-i18n-fencebreaker-ru` |
| `--allow-untrusted` | не проверять подписи (для неподписанного apk-репозитория; в release- и `--dir`-режимах включён всегда) |
| `FB_SKIP_SPACE_CHECK=1` | обойти проверку свободного места (переустановка тех же версий места не требует) |

### Что делает скрипт

1. Проверки окружения: запуск от root; наличие `/etc/openwrt_release`; наличие
   `apk`; архитектура `x86_64`; не менее ~60 МБ свободно в `/` (`df -k /`,
   обходится `FB_SKIP_SPACE_CHECK=1`).
2. В release-режиме: запрашивает GitHub API (последний релиз или тег из
   `--version`), выбирает для каждого пакета самый свежий `.apk`-ассет,
   скачивает их во временный каталог и ставит через
   `apk add --allow-untrusted`.
3. В режиме репозитория: записывает URL в
   `/etc/apk/repositories.d/fencebreaker.conf` (прежнее содержимое
   бэкапится в `.bak` и восстанавливается при любой ошибке); выполняет
   `apk update` и дополнительно проверяет, что индекс реально отдал пакет
   `fencebreaker` — сам `apk update` недоступный репозиторий ошибкой не
   считает; затем `apk add` перечисленных пакетов.
4. В режиме `--dir`: устанавливает найденные `.apk` через
   `apk add --allow-untrusted`; `sing-box` при `--with-singbox` ставится
   отдельно из штатных фидов.
5. `/etc/init.d/fencebreaker enable`; затем старт, если служба ещё не
   работает (при обновлении пакета postinst уже перезапускает работающий
   демон).
6. Ждёт до 30 секунд появления `/var/run/fencebreaker/dns_state.json`
   (демон пишет его при первом apply); если файл не появился — предупреждение
   с подсказкой посмотреть `logread`.
7. Печатает список установленных пакетов и подсказку по дальнейшим шагам.

Конфиг `/etc/config/fencebreaker` создаётся uci-defaults-скриптом пакета
**только при отсутствии** — существующий конфиг при повторном запуске и при
обновлении не перезаписывается.

### Повторный запуск = обновление

Скрипт идемпотентен: повторный запуск обновляет пакеты до новейших доступных
версий и перезапускает службы (postinst пакетов перезапускает работающие
`fencebreaker` и — при наличии `/var/run/fencebreaker/Corefile` —
`fencebreaker-coredns`). Конфигурация сохраняется.

## Первый запуск

По умолчанию служба включена и запущена, но маршрутизации нет: в
поставляемом конфиге примеры тенантов созданы с `enabled '0'`.

- LuCI → **Сеть → fencebreaker → «Тенанты»** — создать тенанта (там же
  страница «Списки»).
- LuCI → **Статус → fencebreaker → «Статус»** — наблюдение (там же —
  «Журнал»).
- Дальнейшая настройка — [CONFIGURATION.md](CONFIGURATION.md).

Минимальный тенант с каналом через отдельный интерфейс (в `networks`
указывается uci-сеть, имя устройства или CIDR):

```sh
cat >> /etc/config/fencebreaker <<'EOF'
config tenant
	option id 'work'
	option egress_mode 'interface'
	option egress_interface 'wan2'
	list networks 'lan'
EOF

/etc/init.d/fencebreaker reload   # SIGHUP: демон перечитывает конфиг
```

После apply демон сгенерирует nft-таблицу `inet fencebreaker`, правила
перехвата DNS, fwmark и маршруты, запустит `fb-coredns` (и `sing-box` —
при VLESS-тенантах).

## Обновление и удаление

Обновление повторным запуском установщика (release-режим — до последнего
релиза, конкретная версия — через `--version`):

```sh
sh install-openwrt.sh
```

Если пакеты ставились из apk-репозитория (`--repo`) и репозиторий оставлен
настроенным:

```sh
apk update && apk upgrade
```

Удаление пакетов:

```sh
apk del fencebreaker luci-app-fencebreaker fencebreaker-coredns
apk del luci-i18n-fencebreaker-ru    # если ставился
```

После удаления остаются (пакеты их не удаляют):

- `/etc/config/fencebreaker` — conffile, сохраняется вместе с вашими
  настройками;
- `/etc/fencebreaker/` — данные демона: кэш списков (`lists/`) и
  скачанный `geoip.dat`;
- `/etc/apk/repositories.d/fencebreaker.conf` — строка репозитория
  (в режиме `--repo`);
- секция `fencebreaker` в `/etc/config/ucitrack` (добавляется uci-defaults).

Полная очистка:

```sh
rm -rf /etc/config/fencebreaker /etc/fencebreaker
rm -f /etc/apk/repositories.d/fencebreaker.conf
uci -q delete ucitrack.@fencebreaker[-1] && uci commit ucitrack
```

`/var/run/fencebreaker/` (runtime-состояние) очищается при перезагрузке;
`sing-box`, если ставился через `--with-singbox` и больше не нужен:
`apk del sing-box`.

## Диагностика установки

Типовые ошибки скрипта установки:

| Сообщение | Причина и действие |
|---|---|
| `мало места в корне: … нужно ~60 МБ` | освободите место в overlay либо `FB_SKIP_SPACE_CHECK=1 sh install-openwrt.sh` (осмысленно только при переустановке тех же версий) |
| `архитектура … не поддерживается в v1 (только x86/64)` | сборки существуют только для x86_64 |
| `не удалось получить сведения о релизе` | нет сети к GitHub, неверно указан `--version` либо релиз ещё не опубликован |
| `в релизе … нет пакета …` | у релиза нет ассета `.apk` (нестандартная сборка релиза); используйте `--dir` |
| `репозиторий … не отдал пакет fencebreaker` | репозиторий недоступен или указан неверный URL; для неподписанного репозитория добавьте `--allow-untrusted` |
| `apk не найден` | на роутере opkg — нужна OpenWrt 24.09+ (рекомендуется 25.12+) |

Проверка состояния после установки:

```sh
/etc/init.d/fencebreaker status        # служба демона
logread | grep -E 'fencebreakerd|fb-coredns'
ls /var/run/fencebreaker/              # dns_state.json, status.json,
                                       # plugin_status.json, Corefile;
                                       # + sing-box.json при VLESS-тенантах
/usr/sbin/fencebreakerd -version       # версия демона
```

Если `/var/run/fencebreaker/dns_state.json` не появился (скрипт предупредит
об этом через 30 секунд) — смотрите `logread | grep fencebreakerd`: скорее
всего демон не смог применить конфигурацию (см. `last_error` в
`/var/run/fencebreaker/status.json`).
