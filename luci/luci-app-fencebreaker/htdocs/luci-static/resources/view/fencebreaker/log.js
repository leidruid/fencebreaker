'use strict';
'require dom';
'require view';
'require rpc';
'require fs';
'require ui';

/*
	System log, filtered to fencebreaker messages, plus the last daemon
	error from /var/run/fencebreaker/status.json.
*/

const STATUS_PATH = '/var/run/fencebreaker/status.json';
const LOG_FILTER = 'fencebreaker';
const LOG_MAX_ROWS = 1000;

const callLogRead = rpc.declare({
	object: 'log',
	method: 'read',
	params: [ 'lines' ],
	expect: { log: [] }
});

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

function retrieveLog() {
	return L.resolveDefault(callLogRead(LOG_MAX_ROWS), []).then(function(entries) {
		const lines = (Array.isArray(entries) ? entries : []).map(function(entry) {
			let ts = entry?.time;

			if (ts && ts < 1e12)
				ts *= 1000;

			const datestr = ts ? new Date(ts).toISOString().replace('T', ' ').replace(/(\.\d+)?Z$/, '') : '';
			const msg = entry?.msg ?? entry?.data ?? '';

			return datestr ? '[%s] %s'.format(datestr, msg) : msg;
		}).filter(function(line) {
			return line.toLowerCase().indexOf(LOG_FILTER) != -1;
		});

		return {
			value: lines.join('\n'),
			rows: lines.length + 1
		};
	}).catch(function(err) {
		ui.addNotification(null,
			E('p', {}, _('Не удалось прочитать системный журнал: %s').format(err.message || err)),
			'error');

		return { value: '', rows: 0 };
	});
}

return view.extend({
	load: function() {
		return Promise.all([
			retrieveLog(),
			readStatus()
		]);
	},

	render: function(data) {
		const log = data[0] || { value: '', rows: 0 };
		const status = data[1];
		const daemon = (status?.daemon && typeof(status.daemon) == 'object') ? status.daemon : {};

		const self = this;

		const logText = E('textarea', {
			'id': 'fencebreaker_log',
			'style': 'font-size:12px',
			'readonly': 'readonly',
			'wrap': 'off',
			'rows': log.rows + 1
		}, [ log.value ]);

		const refreshButton = E('button', {
			'class': 'cbi-button cbi-button-neutral',
			'click': ui.createHandlerFn(self, function() {
				return Promise.all([
					retrieveLog(),
					readStatus()
				]).then(function(res) {
					logText.value = res[0].value;
					logText.rows = res[0].rows + 1;

					const errNode = document.getElementById('fencebreaker_last_error');

					if (errNode) {
						const err = res[1]?.daemon?.last_error;

						dom.content(errNode,
							err ? E('span', { 'class': 'label label-important', 'style': 'white-space:normal' }, err)
							    : E('em', _('нет')));
					}
				});
			})
		}, [ _('Обновить') ]);

		const sections = [];

		sections.push(E('div', { 'class': 'cbi-section' }, [
			E('h3', _('Последняя ошибка демона')),
			E('div', { 'id': 'fencebreaker_last_error' },
				daemon.last_error
					? E('span', { 'class': 'label label-important', 'style': 'white-space:normal' }, daemon.last_error)
					: E('em', _('нет')))
		]));

		sections.push(E('div', { 'id': 'content_fencebreaker_log' }, [
			E('div', { 'style': 'padding-bottom:20px' }, [ refreshButton ]),
			logText
		]));

		return E([], [
			E('h2', _('fencebreaker')),
			E('div', { 'class': 'cbi-map-descr' },
				_('Записи системного журнала (logread), относящиеся к fencebreaker (демон, CoreDNS-плагин, sing-box).')),
			sections
		]);
	},

	handleSaveApply: null,
	handleSave: null,
	handleReset: null
});
