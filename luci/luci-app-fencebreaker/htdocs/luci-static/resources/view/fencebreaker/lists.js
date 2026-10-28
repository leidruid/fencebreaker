'use strict';
'require view';
'require form';
'require fs';
'require poll';
'require ui';

/*
	Subscription lists (UCI "list" sections, see docs/CONTRACTS.md section 1)
	plus the per-list download state from /var/run/fencebreaker/status.json.
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
			return null;
		}
	});
}

function stateBadge(state) {
	const ok = (state == 'ok');
	const bad = (state == 'error');

	return E('span', {
		'class': ok ? 'label label-success' : (bad ? 'label label-important' : 'label')
	}, state || '-');
}

function renderListStatus(lists) {
	const rows = (Array.isArray(lists) ? lists : []).map(function(l) {
		return E('tr', { 'class': 'tr' }, [
			E('td', { 'class': 'td left' }, l.name || '-'),
			E('td', { 'class': 'td left' }, stateBadge(l.state)),
			E('td', { 'class': 'td left' }, l.updated || '-'),
			E('td', { 'class': 'td left' }, l.nets != null ? String(l.nets) : '-'),
			E('td', { 'class': 'td left' }, l.domains != null ? String(l.domains) : '-'),
			E('td', { 'class': 'td left' },
				l.error ? E('span', { 'class': 'label label-important', 'style': 'white-space:normal' }, l.error) : E('em', _('нет')))
		]);
	});

	/* LuCI dom.append() does not flatten nested arrays: keep children flat */
	const bodyRows = rows.length ? rows : [
		E('tr', { 'class': 'tr' },
			E('td', { 'class': 'td left', 'colspan': 6 }, E('em', _('Нет данных: файл состояния недоступен или списки ещё не загружены.'))))
	];

	return E('div', { 'class': 'cbi-section' }, [
		E('h3', _('Состояние списков')),
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

return view.extend({
	load: function() {
		return readStatus();
	},

	render: function(status) {
		let m, s, o;

		m = new form.Map('fencebreaker', _('fencebreaker'),
			_('Списки адресов и доменов: ручные списки и подписки по URL. Содержимое списков используется тенантами (include_lists / exclude_lists).'));

		s = m.section(form.GridSection, 'list', _('Списки'));
		s.addremove = true;
		s.anonymous = true;
		s.sortable = true;
		s.nodescriptions = true;

		o = s.option(form.Value, 'name', _('Имя'),
			_('Уникальное имя списка, на него ссылаются тенанты.'));
		o.rmempty = false;
		o.datatype = 'and(uciname, maxlength(64))';

		o = s.option(form.Value, 'url', _('URL подписки'),
			_('URL подписки (http(s):// или file://), текстовый список — одна запись в строке. Оставьте поле пустым, чтобы сделать список ручным: тогда его содержимое задаётся полем «Содержимое» ниже.'));
		o.datatype = 'url';
		o.placeholder = 'https://example.com/list.txt';

		o = s.option(form.Value, 'refresh', _('Период обновления, часы'),
			_('Интервал обновления подписки в часах. 0 — обновлять только вручную; для ручных списков (без URL) не используется.'));
		o.datatype = 'uinteger';
		o.default = '24';
		o.placeholder = '24';

		o = s.option(form.TextValue, 'content', _('Содержимое'),
			_('Записи списка — по одной в строке: домены (example.com), подсети (1.2.3.0/24) и адреса (5.6.7.8), вперемешку; строки с # — комментарии. Используется только для ручных списков (URL пуст); подписки игнорируют это поле и загружают содержимое по URL.'));
		o.rows = 12;
		o.placeholder = 'example.com\n1.2.3.0/24\n5.6.7.8\n# комментарий';

		const statusNode = E('div', {}, renderListStatus(status && status.lists));

		poll.add(function() {
			return readStatus().then(function(next) {
				const node = statusNode.firstChild;

				if (node)
					node.replaceWith(renderListStatus(next && next.lists));
			});
		}, 5);

		return m.render().then(function(nodes) {
			return E([], [ nodes, statusNode ]);
		});
	},

});
