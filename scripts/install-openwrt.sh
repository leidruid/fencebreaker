#!/bin/sh
# fencebreaker — автоустановка на OpenWrt 25.12+ (apk, x86/64).
#
# Пакеты берутся из GitHub Releases проекта (по умолчанию — последний релиз;
# конкретная версия — через --version TAG) либо из apk-репозитория
# (--repo URL) или локального каталога с собранными .apk (--dir DIR).
#
# Использование:
#   ./install-openwrt.sh [--version TAG | --repo URL | --dir DIR]
#                        [--with-singbox] [--no-i18n] [--allow-untrusted] [-h]
#
#   --version TAG        релиз-тег GitHub (например v0.1.0);
#                        по умолчанию — последний релиз
#   --repo URL           apk-репозиторий fencebreaker
#   --dir DIR            каталог с .apk-файлами (out/apk/.../fencebreaker)
#   --with-singbox       дополнительно установить sing-box (нужен для
#                        VLESS-каналов тенантов; ставится из официального
#                        packages-feed)
#   --no-i18n            не ставить luci-i18n-fencebreaker-ru
#   --allow-untrusted    не проверять подписи (для неподписанного репозитория;
#                        в release- и --dir-режимах включён всегда)
#
# Скрипт идемпотентен: повторный запуск обновляет пакеты. Существующий
# /etc/config/fencebreaker не перезаписывается.
#
# Удаление: apk del fencebreaker luci-app-fencebreaker fencebreaker-coredns
set -eu

REPO_CONF=/etc/apk/repositories.d/fencebreaker.conf
GH_REPO=leidruid/fencebreaker

usage() {
	awk 'NR>1 { if ($0 ~ /^set -eu/) exit; sub(/^# ?/, ""); print }' "$0"
	exit "${1:-0}"
}

die() {
	echo "ошибка: $*" >&2
	exit 1
}

RELEASE_TAG="latest"
REPO=""
DIR=""
WITH_SINGBOX=0
NO_I18N=0
UNTRUSTED=0

while [ $# -gt 0 ]; do
	case "$1" in
	--version)
		[ $# -ge 2 ] || die "--version требует аргумент TAG (например v0.1.0)"
		RELEASE_TAG="$2"
		shift 2
		;;
	--repo)
		[ $# -ge 2 ] || die "--repo требует аргумент URL"
		REPO="$2"
		shift 2
		;;
	--dir)
		[ $# -ge 2 ] || die "--dir требует аргумент DIR"
		DIR="$2"
		shift 2
		;;
	--with-singbox)
		WITH_SINGBOX=1
		shift
		;;
	--no-i18n)
		NO_I18N=1
		shift
		;;
	--allow-untrusted)
		UNTRUSTED=1
		shift
		;;
	-h | --help)
		usage 0
		;;
	*)
		echo "неизвестный аргумент: $1" >&2
		usage 2
		;;
	esac
done

MODE="release"
[ -n "$DIR" ] && MODE="dir"
[ -n "$REPO" ] && MODE="repo"

# ---- проверки окружения ----------------------------------------------------

[ "$(id -u)" -eq 0 ] || die "запустите от root (ssh root@router)"

[ -f /etc/openwrt_release ] || die "это не OpenWrt (/etc/openwrt_release не найден)"

# fencebreaker v1 требует apk (OpenWrt 24.09+; сборки под 25.12)
command -v apk >/dev/null 2>&1 ||
	die "apk не найден: нужна OpenWrt 24.09+ (25.12+ рекомендуется); opkg не поддерживается"

# shellcheck disable=SC1091 # router file, absent in CI/dev environments
. /etc/openwrt_release
echo "OpenWrt ${DISTRIB_RELEASE:-?} / ${DISTRIB_ARCH:-?} / ${DISTRIB_TARGET:-?}"

case "${DISTRIB_ARCH:-}" in
x86_64) ;;
*)
	die "архитектура ${DISTRIB_ARCH:-неизвестна} не поддерживается в v1 (только x86/64)"
	;;
esac

# fencebreaker-coredns разворачивается в ~35 МБ + зависимости; проверку можно
# обойти (переустановка тех же версий места не требует) через
# FB_SKIP_SPACE_CHECK=1
if [ -z "${FB_SKIP_SPACE_CHECK:-}" ]; then
	avail_kb="$(df -k / | awk 'NR==2 {print $4}')"
	[ -n "$avail_kb" ] || avail_kb=99999999
	[ "$avail_kb" -ge 61440 ] ||
		die "мало места в корне: ${avail_kb} КБ свободно, нужно ~60 МБ"
fi

# ---- установка --------------------------------------------------------------

# выбирает из строк-URL/имён файлов пакет с наибольшей версией; шаблон
# "$1"-[0-9]* отсекает пакеты с более длинными именами
# (fencebreaker-0.1.0-r9.apk, но не fencebreaker-coredns-...).
latest_match() {
	sort -V | tail -n 1
}

skip_i18n() {
	[ "$1" = "luci-i18n-fencebreaker-ru" ] && [ "$NO_I18N" -eq 1 ]
}

# Установка из GitHub Releases: ассеты релиза — .apk-файлы пакетов; имена
# содержат версии пакетов, для каждого берём самый свежий ассет.
install_release() {
	command -v curl >/dev/null 2>&1 ||
		die "curl не найден (нужен для загрузки из GitHub Releases)"

	if [ "$RELEASE_TAG" = "latest" ]; then
		api="https://api.github.com/repos/$GH_REPO/releases/latest"
	else
		api="https://api.github.com/repos/$GH_REPO/releases/tags/$RELEASE_TAG"
	fi

	json="$(curl -sfL "$api")" ||
		die "не удалось получить сведения о релизе: $api (проверьте тег --version)"

	asset_urls() {
		printf '%s\n' "$json" |
			grep -o '"browser_download_url"[[:space:]]*:[[:space:]]*"[^"]*"' |
			sed 's/.*"\(https[^"]*\)"$/\1/'
	}

	DL="$(mktemp -d /tmp/fencebreaker-apk.XXXXXX)"

	echo "релиз: ${RELEASE_TAG} ($GH_REPO)"
	for name in fencebreaker-coredns fencebreaker luci-app-fencebreaker luci-i18n-fencebreaker-ru; do
		skip_i18n "$name" && continue

		url="$(asset_urls | grep "/${name}-[0-9][^/]*\.apk$" | latest_match)"

		if [ -z "$url" ]; then
			if [ "$name" = "luci-i18n-fencebreaker-ru" ]; then
				echo "предупреждение: ассет luci-i18n-fencebreaker-ru в релизе не найден — пропускаю (интерфейс будет английским)" >&2
				continue
			fi
			rm -rf "$DL"
			die "в релизе ${RELEASE_TAG} нет пакета $name (*.apk)"
		fi

		f="$DL/$(basename "$url")"
		echo "  пакет: $(basename "$f")"
		curl -sfL -o "$f" "$url" || { rm -rf "$DL"; die "не удалось скачать $url"; }
		set -- "$@" "$f"
	done

	apk add --allow-untrusted "$@"
	rm -rf "$DL"
	[ "$WITH_SINGBOX" -eq 1 ] && apk add sing-box
}

# Установка из apk-репозитория по URL.
install_repo() {
	echo "репозиторий: $REPO"
	mkdir -p /etc/apk/repositories.d
	[ -f "$REPO_CONF" ] && cp "$REPO_CONF" "$REPO_CONF.bak"
	echo "$REPO" >"$REPO_CONF"

	# apk update не считает недоступный репозиторий ошибкой (предупреждение),
	# поэтому дополнительно проверяем, что индекс реально отдал пакет.
	restore_repo() {
		if [ -f "$REPO_CONF.bak" ]; then
			mv -f "$REPO_CONF.bak" "$REPO_CONF"
		else
			rm -f "$REPO_CONF"
		fi
	}
	apk_flags=""
	[ "$UNTRUSTED" -eq 1 ] && apk_flags="--allow-untrusted"

	pkgs="fencebreaker luci-app-fencebreaker"
	[ "$NO_I18N" -eq 0 ] && pkgs="$pkgs luci-i18n-fencebreaker-ru"
	[ "$WITH_SINGBOX" -eq 1 ] && pkgs="$pkgs sing-box"

	# shellcheck disable=SC2086 # флаги и пакетный список — намеренный word-splitting
	if ! apk update $apk_flags ||
		[ -z "$(apk search -x fencebreaker 2>/dev/null)" ]; then
		restore_repo
		die "репозиторий $REPO не отдал пакет fencebreaker (проверьте URL/подпись; --allow-untrusted — для неподписанных)"
	fi
	# shellcheck disable=SC2086 # флаги и пакетный список — намеренный word-splitting
	if ! apk add $apk_flags $pkgs; then
		restore_repo
		die "apk add не смог установить пакеты из $REPO"
	fi
}

# Установка из локального каталога с собранными .apk.
install_dir() {
	# локальные .apk: берём новейшую версию каждого пакета в каталоге.
	[ -d "$DIR" ] || die "каталог не найден: $DIR"

	for name in fencebreaker-coredns fencebreaker luci-app-fencebreaker luci-i18n-fencebreaker-ru; do
		skip_i18n "$name" && continue

		# shellcheck disable=SC2012 # package names are apk-controlled (no spaces); -1+sort -V picks the newest
		f="$(ls -1 "$DIR"/"$name"-[0-9]*.apk 2>/dev/null | latest_match)"

		if [ -z "$f" ]; then
			if [ "$name" = "luci-i18n-fencebreaker-ru" ]; then
				echo "предупреждение: luci-i18n-fencebreaker-ru не найден в $DIR — пропускаю (интерфейс будет английским)" >&2
				continue
			fi
			die "в $DIR нет пакета $name (*.apk)"
		fi
		set -- "$@" "$f"
	done

	echo "установка из $DIR:"
	printf '  %s\n' "$@"
	apk add --allow-untrusted "$@"
	[ "$WITH_SINGBOX" -eq 1 ] && apk add sing-box
}

case "$MODE" in
release) install_release ;;
repo) install_repo ;;
dir) install_dir ;;
esac

# ---- запуск ------------------------------------------------------------------

/etc/init.d/fencebreaker enable
# postinst уже перезапускает работающий демон; стартуем, если не работает
/etc/init.d/fencebreaker running || /etc/init.d/fencebreaker start

# ждём первую генерацию состояния CoreDNS (демон пишет его при apply)
i=0
while [ $i -lt 30 ]; do
	[ -f /var/run/fencebreaker/dns_state.json ] && break
	i=$((i + 1))
	sleep 1
done
[ -f /var/run/fencebreaker/dns_state.json ] ||
	echo "предупреждение: /var/run/fencebreaker/dns_state.json не появился за 30 с — проверьте logread | grep fencebreakerd" >&2

echo
echo "=== fencebreaker установлен ==="
apk info -e fencebreaker fencebreaker-coredns luci-app-fencebreaker 2>/dev/null |
	sed 's/^/  пакет: /'
[ -f /etc/config/fencebreaker ] &&
	echo "  конфиг: /etc/config/fencebreaker (не перезаписан при обновлении)"
echo
echo "Дальше:"
echo "  1. LuCI: Сеть → fencebreaker → «Тенанты» — создать тенанта;"
echo "     Статус → fencebreaker → «Статус» — наблюдение."
echo "  2. Или UCI: см. README (Быстрый старт) и docs/CONFIGURATION.md."
echo "  3. Для VLESS-канала: ./install-openwrt.sh --with-singbox (если ещё не ставили)."
