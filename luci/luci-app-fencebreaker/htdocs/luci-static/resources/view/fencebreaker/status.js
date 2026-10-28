'use strict';
'require dom';
'require view';
'require poll';
'require fs';
'require ui';

/*
	Rendering of /var/run/fencebreaker/status.json as produced by the
	fencebreaker daemon, see docs/CONTRACTS.md section 5.
*/

const STATUS_PATH = '/var/run/fencebreaker/status.json';

function readStatus() {
	return L.resolveDefault(fs.read_direct(STATUS_PATH), null).then(function(content) {
		if (content == null)
			return null;

		try {
			return JSON.parse(content);
		}
		catch (e) {
			return { __invalid: content };
		}
	});
}

function fmtUptime(sec) {
	if (sec == null || isNaN(sec) || sec < 0)
		return '-';

	sec = Math.floor(sec);

	const days = Math.floor(sec / 86400);
	const hours = Math.floor((sec % 86400) / 3600);
	const mins = Math.floor((sec % 3600) / 60);

	if (days > 0)
		return '%d %s %d %s %d %s'.format(days, _('дн.'), hours, _('ч.'), mins, _('мин.'));
	else if (hours > 0)
		return '%d %s %d %s'.format(hours, _('ч.'), mins, _('мин.'));
	else
		return '%d %s'.format(mins, _('мин.'));
}

function fmtDate(v) {
	if (!v)
		return '-';
	return String(v);
}

function stateBadge(state) {
	const ok = (state == 'ok' || state == 'up');
	const bad = (state == 'error' || state == 'down');

	return E('span', {
		'class': ok ? 'label label-success' : (bad ? 'label label-important' : 'label')
	}, state || '-');
}

function row(label, value) {
	return E('tr', { 'class': 'tr' }, [
		E('td', { 'class': 'td left', 'width': '33%' }, [ label ]),
		E('td', { 'class': 'td left' }, [ value ])
	]);
}

function renderDaemon(d) {
	return E('div', { 'class': 'cbi-section' }, [
		E('h3', _('Демон')),
		E('table', { 'class': 'table' }, [
			E('tr', { 'class': 'tr table-titles' }, [
				E('th', { 'class': 'th left' }, _('Параметр')),
				E('th', { 'class': 'th left' }, _('Значение'))
			]),
			row(_('Время работы'), fmtUptime(d.uptime_sec)),
			row(_('Генерация DNS-наборов'), d.generation != null ? String(d.generation) : '-'),
			row(_('Последняя ошибка'),
				d.last_error
					? E('span', { 'class': 'label label-important', 'style': 'white-space:normal' }, d.last_error)
					: E('em', _('нет'))
			)
		])
	]);
}

function renderTenant(t) {
	const sets = (t.sets && typeof(t.sets) == 'object') ? t.sets : {};
	const dns = (t.dns && typeof(t.dns) == 'object') ? t.dns : {};
	const keeper = (dns.keeper && typeof(dns.keeper) == 'object') ? dns.keeper : null;
	const egress = (t.egress && typeof(t.egress) == 'object') ? t.egress : {};
	const errors = Array.isArray(t.errors) ? t.errors : [];

	const title = [
		(t.name || t.id || '-'),
		' ',
		t.enabled ? stateBadge('ok') : stateBadge('disabled')
	];

	if (egress.state)
		title.push(' ', stateBadge(egress.state));

	const learnedDns = (sets.inc_dns != null ? String(sets.inc_dns) : '-') +
		(sets.exc_dns > 0 ? ', ' + _('исключено: %d').format(sets.exc_dns) : '');

	const rows = [
		row(_('Канал'), E([], [
			'%s: %s'.format(egress.mode || '-', egress.target || '-'),
			' ',
			egress.state ? stateBadge(egress.state) : E([])
		])),
		row(_('Обучено DNS-адресов'), learnedDns),
		row(_('Счётчики DNS'), _('запросов: %s, обучено: %s, ошибок upstream: %s').format(
			String(dns.queries || 0),
			String(dns.learned || 0),
			String(dns.upstream_errors || 0)
		))
	];

	if (keeper)
		rows.push(row(_('Продление адресов'), _('доменов: %s, продлений: %s, ошибок: %s, последнее: %s').format(
			String(keeper.domains || 0),
			String(keeper.refreshes || 0),
			String(keeper.errors || 0),
			fmtDate(keeper.last_refresh)
		)));

	if (errors.length)
		rows.push(row(_('Ошибки'),
			E('div', { 'style': 'white-space:pre-line' }, errors.join('\n'))));

	return E('div', { 'class': 'cbi-section' }, [
		E('h3', title),
		E('table', { 'class': 'table' }, rows)
	]);
}

function renderLists(lists) {
	const rows = (Array.isArray(lists) ? lists : []).map(function(l) {
		return E('tr', { 'class': 'tr' }, [
			E('td', { 'class': 'td left' }, l.name || '-'),
			E('td', { 'class': 'td left' }, stateBadge(l.state)),
			E('td', { 'class': 'td left' }, fmtDate(l.updated)),
			E('td', { 'class': 'td left' }, l.nets != null ? String(l.nets) : '-'),
			E('td', { 'class': 'td left' }, l.domains != null ? String(l.domains) : '-'),
			E('td', { 'class': 'td left' },
				l.error ? E('span', { 'class': 'label label-important', 'style': 'white-space:normal' }, l.error) : E('em', _('нет')))
		]);
	});

	/* LuCI dom.append() does not flatten nested arrays: keep children flat */
	const bodyRows = rows.length ? rows : [
		E('tr', { 'class': 'tr' },
			E('td', { 'class': 'td left', 'colspan': 6 }, E('em', _('Списки не определены'))))
	];

	return E('div', { 'class': 'cbi-section' }, [
		E('h3', _('Списки')),
		E('table', { 'class': 'table' }, [
			E('tr', { 'class': 'tr table-titles' }, [
				E('th', { 'class': 'th left' }, _('Имя')),
				E('th', { 'class': 'th left' }, _('Состояние')),
				E('th', { 'class': 'th left' }, _('Обновлён')),
				E('th', { 'class': 'th left' }, _('Подсетей')),
				E('th', { 'class': 'th left' }, _('Доменов')),
				E('th', { 'class': 'th left' }, _('Ошибка'))
			]),
			...bodyRows
		])
	]);
}

function renderGeodata(g) {
	g = (g && typeof(g) == 'object') ? g : {};

	return E('div', { 'class': 'cbi-section' }, [
		E('h3', _('Geodata')),
		E('table', { 'class': 'table' }, [
			row(_('Состояние'), stateBadge(g.state)),
			row(_('Обновлено'), fmtDate(g.updated)),
			row(_('Записей'), g.entries != null ? String(g.entries) : '-'),
			row(_('Ошибка'),
				g.error ? E('span', { 'class': 'label label-important', 'style': 'white-space:normal' }, g.error) : E('em', _('нет')))
		])
	]);
}

function renderBody(status) {
	if (status == null) {
		return E('div', { 'class': 'alert-message warning' }, [
			E('p', {}, [ E('strong', {}, _('Демон fencebreaker не запущен.')),
				' ',
				_('Файл состояния %s недоступен.').format(STATUS_PATH) ]),
			E('p', _('Служба создаёт файл состояния не позднее чем через 10 секунд после старта. Проверить состояние службы можно командой «/etc/init.d/fencebreaker status» или на странице «Система → Стартовые скрипты».'))
		]);
	}

	if (status.__invalid != null) {
		return E('div', { 'class': 'alert-message error' }, [
			E('p', _('Не удалось разобрать файл состояния %s (некорректный JSON).').format(STATUS_PATH)),
			E('pre', { 'style': 'white-space:pre-wrap' }, String(status.__invalid).trim())
		]);
	}

	const daemon = (status.daemon && typeof(status.daemon) == 'object') ? status.daemon : {};
	const tenants = Array.isArray(status.tenants) ? status.tenants : [];

	return E([], [
		renderDaemon(daemon),
		...tenants.map(renderTenant),
		tenants.length ? E([]) : E('div', { 'class': 'alert-message message' },
			E('p', _('Тенанты не определены. Добавьте их на странице «Сеть → fencebreaker → Тенанты».'))),
		renderLists(status.lists),
		renderGeodata(status.geodata)
	]);
}

return view.extend({
	load: function() {
		return readStatus();
	},

	render: function(status) {
		const body = E('div', {}, renderBody(status));

		poll.add(L.bind(function() {
			return readStatus().then(L.bind(function(next) {
				dom.content(body, renderBody(next));
			}, this));
		}, this), 5);

		return E([], [
			E('h2', _('fencebreaker')),
			E('div', { 'class': 'cbi-map-descr' },
				_('Состояние демона, тенантов, nft-наборов и подписок списков (обновляется каждые 5 секунд).')),
			body
		]);
	},

	handleSaveApply: null,
	handleSave: null,
	handleReset: null
});
