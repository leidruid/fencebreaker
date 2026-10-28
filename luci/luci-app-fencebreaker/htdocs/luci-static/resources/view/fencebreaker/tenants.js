'use strict';
'require view';
'require form';
'require uci';
'require ui';
'require rpc';

/*
	Configuration of multi-tenant policy routing, see docs/CONTRACTS.md
	section 1 (UCI: /etc/config/fencebreaker).
*/

const getDeviceStatus = rpc.declare({
	object: 'network.device',
	method: 'status'
});

/* ISO 3166-1 alpha-2 → название страны (ru) */
const GEO_COUNTRIES = {
	AD: 'Андорра',
	AE: 'ОАЭ',
	AF: 'Афганистан',
	AG: 'Антигуа и Барбуда',
	AI: 'Ангилья',
	AL: 'Албания',
	AM: 'Армения',
	AO: 'Ангола',
	AQ: 'Антарктида',
	AR: 'Аргентина',
	AS: 'Американское Самоа',
	AT: 'Австрия',
	AU: 'Австралия',
	AW: 'Аруба',
	AX: 'Аландские о-ва',
	AZ: 'Азербайджан',
	BA: 'Босния и Герцеговина',
	BB: 'Барбадос',
	BD: 'Бангладеш',
	BE: 'Бельгия',
	BF: 'Буркина-Фасо',
	BG: 'Болгария',
	BH: 'Бахрейн',
	BI: 'Бурунди',
	BJ: 'Бенин',
	BL: 'Сен-Бартелеми',
	BM: 'Бермудские о-ва',
	BN: 'Бруней-Даруссалам',
	BO: 'Боливия',
	BQ: 'Бонэйр, Синт-Эстатиус и Саба',
	BR: 'Бразилия',
	BS: 'Багамы',
	BT: 'Бутан',
	BV: 'о-в Буве',
	BW: 'Ботсвана',
	BY: 'Беларусь',
	BZ: 'Белиз',
	CA: 'Канада',
	CC: 'Кокосовые о-ва',
	CD: 'Конго - Киншаса',
	CF: 'Центрально-Африканская Республика',
	CG: 'Конго - Браззавиль',
	CH: 'Швейцария',
	CI: 'Кот-д’Ивуар',
	CK: 'Острова Кука',
	CL: 'Чили',
	CM: 'Камерун',
	CN: 'Китай',
	CO: 'Колумбия',
	CR: 'Коста-Рика',
	CU: 'Куба',
	CV: 'Кабо-Верде',
	CW: 'Кюрасао',
	CX: 'о-в Рождества',
	CY: 'Кипр',
	CZ: 'Чехия',
	DE: 'Германия',
	DJ: 'Джибути',
	DK: 'Дания',
	DM: 'Доминика',
	DO: 'Доминиканская Республика',
	DZ: 'Алжир',
	EC: 'Эквадор',
	EE: 'Эстония',
	EG: 'Египет',
	EH: 'Западная Сахара',
	ER: 'Эритрея',
	ES: 'Испания',
	ET: 'Эфиопия',
	FI: 'Финляндия',
	FJ: 'Фиджи',
	FK: 'Фолклендские о-ва',
	FM: 'Федеративные Штаты Микронезии',
	FO: 'Фарерские о-ва',
	FR: 'Франция',
	GA: 'Габон',
	GB: 'Великобритания',
	GD: 'Гренада',
	GE: 'Грузия',
	GF: 'Французская Гвиана',
	GG: 'Гернси',
	GH: 'Гана',
	GI: 'Гибралтар',
	GL: 'Гренландия',
	GM: 'Гамбия',
	GN: 'Гвинея',
	GP: 'Гваделупа',
	GQ: 'Экваториальная Гвинея',
	GR: 'Греция',
	GS: 'Южная Георгия и Южные Сандвичевы о-ва',
	GT: 'Гватемала',
	GU: 'Гуам',
	GW: 'Гвинея-Бисау',
	GY: 'Гайана',
	HK: 'Гонконг (САР)',
	HM: 'о-ва Херд и Макдональд',
	HN: 'Гондурас',
	HR: 'Хорватия',
	HT: 'Гаити',
	HU: 'Венгрия',
	ID: 'Индонезия',
	IE: 'Ирландия',
	IL: 'Израиль',
	IM: 'о-в Мэн',
	IN: 'Индия',
	IO: 'Британская территория в Индийском океане',
	IQ: 'Ирак',
	IR: 'Иран',
	IS: 'Исландия',
	IT: 'Италия',
	JE: 'Джерси',
	JM: 'Ямайка',
	JO: 'Иордания',
	JP: 'Япония',
	KE: 'Кения',
	KG: 'Киргизия',
	KH: 'Камбоджа',
	KI: 'Кирибати',
	KM: 'Коморы',
	KN: 'Сент-Китс и Невис',
	KP: 'КНДР',
	KR: 'Республика Корея',
	KW: 'Кувейт',
	KY: 'Острова Кайман',
	KZ: 'Казахстан',
	LA: 'Лаос',
	LB: 'Ливан',
	LC: 'Сент-Люсия',
	LI: 'Лихтенштейн',
	LK: 'Шри-Ланка',
	LR: 'Либерия',
	LS: 'Лесото',
	LT: 'Литва',
	LU: 'Люксембург',
	LV: 'Латвия',
	LY: 'Ливия',
	MA: 'Марокко',
	MC: 'Монако',
	MD: 'Молдова',
	ME: 'Черногория',
	MF: 'Сен-Мартен',
	MG: 'Мадагаскар',
	MH: 'Маршалловы Острова',
	MK: 'Северная Македония',
	ML: 'Мали',
	MM: 'Мьянма (Бирма)',
	MN: 'Монголия',
	MO: 'Макао (САР)',
	MP: 'Северные Марианские о-ва',
	MQ: 'Мартиника',
	MR: 'Мавритания',
	MS: 'Монтсеррат',
	MT: 'Мальта',
	MU: 'Маврикий',
	MV: 'Мальдивы',
	MW: 'Малави',
	MX: 'Мексика',
	MY: 'Малайзия',
	MZ: 'Мозамбик',
	NA: 'Намибия',
	NC: 'Новая Каледония',
	NE: 'Нигер',
	NF: 'о-в Норфолк',
	NG: 'Нигерия',
	NI: 'Никарагуа',
	NL: 'Нидерланды',
	NO: 'Норвегия',
	NP: 'Непал',
	NR: 'Науру',
	NU: 'Ниуэ',
	NZ: 'Новая Зеландия',
	OM: 'Оман',
	PA: 'Панама',
	PE: 'Перу',
	PF: 'Французская Полинезия',
	PG: 'Папуа — Новая Гвинея',
	PH: 'Филиппины',
	PK: 'Пакистан',
	PL: 'Польша',
	PM: 'Сен-Пьер и Микелон',
	PN: 'о-ва Питкэрн',
	PR: 'Пуэрто-Рико',
	PS: 'Палестинские территории',
	PT: 'Португалия',
	PW: 'Палау',
	PY: 'Парагвай',
	QA: 'Катар',
	RE: 'Реюньон',
	RO: 'Румыния',
	RS: 'Сербия',
	RU: 'Россия',
	RW: 'Руанда',
	SA: 'Саудовская Аравия',
	SB: 'Соломоновы Острова',
	SC: 'Сейшельские Острова',
	SD: 'Судан',
	SE: 'Швеция',
	SG: 'Сингапур',
	SH: 'о-в Св. Елены',
	SI: 'Словения',
	SJ: 'Шпицберген и Ян-Майен',
	SK: 'Словакия',
	SL: 'Сьерра-Леоне',
	SM: 'Сан-Марино',
	SN: 'Сенегал',
	SO: 'Сомали',
	SR: 'Суринам',
	SS: 'Южный Судан',
	ST: 'Сан-Томе и Принсипи',
	SV: 'Сальвадор',
	SX: 'Синт-Мартен',
	SY: 'Сирия',
	SZ: 'Эсватини',
	TC: 'о-ва Тёркс и Кайкос',
	TD: 'Чад',
	TF: 'Французские Южные территории',
	TG: 'Того',
	TH: 'Таиланд',
	TJ: 'Таджикистан',
	TK: 'Токелау',
	TL: 'Восточный Тимор',
	TM: 'Туркменистан',
	TN: 'Тунис',
	TO: 'Тонга',
	TR: 'Турция',
	TT: 'Тринидад и Тобаго',
	TV: 'Тувалу',
	TW: 'Тайвань',
	TZ: 'Танзания',
	UA: 'Украина',
	UG: 'Уганда',
	UM: 'Внешние малые о-ва (США)',
	US: 'Соединенные Штаты',
	UY: 'Уругвай',
	UZ: 'Узбекистан',
	VA: 'Ватикан',
	VC: 'Сент-Винсент и Гренадины',
	VE: 'Венесуэла',
	VG: 'Виргинские о-ва (Великобритания)',
	VI: 'Виргинские о-ва (США)',
	VN: 'Вьетнам',
	VU: 'Вануату',
	WF: 'Уоллис и Футуна',
	WS: 'Самоа',
	YE: 'Йемен',
	YT: 'Майотта',
	ZA: 'Южно-Африканская Республика',
	ZM: 'Замбия',
	ZW: 'Зимбабве',
};

/*
	UCI list options are edited as textareas: one entry per line (domains
	or subnets). wireList() binds a form.TextValue to the list option and
	validates every line.
*/
function textToList(value) {
	return String(value == null ? '' : value).split(/\r?\n/)
		.map(function(line) { return line.trim(); })
		.filter(function(line) { return line.length > 0; });
}

function listToText(value) {
	if (Array.isArray(value))
		return value.join('\n');

	return (value == null) ? '' : String(value);
}

function isIPv4(s) {
	const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s);

	if (!m)
		return false;

	for (let i = 1; i <= 4; i++)
		if (Number(m[i]) > 255)
			return false;

	return true;
}

function isCIDR4(s) {
	const i = s.lastIndexOf('/');

	if (i < 0)
		return false;

	const mask = Number(s.substring(i + 1));

	return isIPv4(s.substring(0, i)) &&
		/^\d{1,2}$/.test(s.substring(i + 1)) &&
		mask <= 32;
}

function isHostname(s) {
	/* leading dot = bare suffix (forward zones), strip before checking */
	if (s[0] == '.')
		s = s.substring(1);

	if (!s.length || s.length > 253)
		return false;

	for (const label of s.split('.')) {
		if (!label.length || label.length > 63)
			return false;

		if (!/^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?$/.test(label))
			return false;
	}

	return true;
}

const CHECKS = {
	cidr4:    { fn: isCIDR4,          hint: _('подсеть в виде CIDR, например 1.2.3.0/24') },
	hostname: { fn: isHostname,       hint: _('домен, например example.com') }
};

/* dns_upstream: формат адреса проверяется регулярками, разбор — на стороне
   CoreDNS-плагина (11 поддерживаемых форм, см. описание опции) */

const IPV6_RE = /^(([0-9a-fA-F]{1,4}:){7}[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,7}:|([0-9a-fA-F]{1,4}:){1,6}:[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,5}(:[0-9a-fA-F]{1,4}){1,2}|([0-9a-fA-F]{1,4}:){1,4}(:[0-9a-fA-F]{1,4}){1,3}|([0-9a-fA-F]{1,4}:){1,3}(:[0-9a-fA-F]{1,4}){1,4}|([0-9a-fA-F]{1,4}:){1,2}(:[0-9a-fA-F]{1,4}){1,5}|[0-9a-fA-F]{1,4}:((:[0-9a-fA-F]{1,4}){1,6})|:((:[0-9a-fA-F]{1,4}){1,7}|:)|::(ffff(:0{1,4})?:)?((25[0-5]|(2[0-4]|1?[0-9])?[0-9])\.){3}(25[0-5]|(2[0-4]|1?[0-9])?[0-9])|([0-9a-fA-F]{1,4}:){1,4}:((25[0-5]|(2[0-4]|1?[0-9])?[0-9])\.){3}(25[0-5]|(2[0-4]|1?[0-9])?[0-9]))$/;

function isIPv6(s) {
	return IPV6_RE.test(s);
}

/* порт не указан — норма; указан — десятичный, 1..65535 */
function isUpstreamPort(p) {
	if (p == null || p == '')
		return true;

	return /^\d+$/.test(p) && Number(p) >= 1 && Number(p) <= 65535;
}

/* «host[:port]»: IPv4, [IPv6](:порт), «голый» IPv6 без порта или имя хоста */
function isUpstreamAuthority(s) {
	if (!s.length)
		return false;

	const br = /^\[([^\]]+)\](?::(\d{1,5}))?$/.exec(s);

	if (br)
		return isIPv6(br[1]) && isUpstreamPort(br[2]);

	/* больше одного двоеточия вне скобок — IPv6 без порта */
	if ((s.match(/:/g) || []).length > 1)
		return isIPv6(s);

	const m = /^([^\[\]:]+)(?::(\d{1,5}))?$/.exec(s);

	if (!m)
		return false;

	return isUpstreamPort(m[2]) &&
		(isIPv4(m[1]) || (m[1][0] != '.' && isHostname(m[1])));
}

/* одна запись dns_upstream в любом из 11 поддерживаемых форматов */
function isDNSUpstreamAddr(s) {
	s = String(s == null ? '' : s).trim();

	if (!s.length)
		return false;

	/* DNS Stamp (DNSCrypt/DoH): непустой base64url-хвост */
	if (/^sdns:\/\/[A-Za-z0-9_-]{8,}={0,2}$/.test(s))
		return true;

	const m = /^(udp|tcp|tls|quic|https|h3):\/\/(.+)$/i.exec(s);

	if (m) {
		const scheme = m[1].toLowerCase();
		let auth = m[2];

		/* https/h3 могут нести путь, начинающийся с "/" */
		if (scheme == 'https' || scheme == 'h3') {
			const i = auth.indexOf('/');

			if (i >= 0) {
				if (/\s/.test(auth.substring(i + 1)))
					return false;

				auth = auth.substring(0, i);
			}
		} else if (auth.indexOf('/') >= 0) {
			return false;
		}

		return isUpstreamAuthority(auth);
	}

	/* без схемы — «голый» адрес (UDP) */
	return isUpstreamAuthority(s);
}

/*
	Turn a freshly created form.TextValue option into a one-per-line editor
	for the UCI list option of the same name. wire() must be called before
	the map is rendered; kind selects the per-line validation.
*/
function wireList(o, kind, opts) {
	opts = opts || {};

	/* AbstractValue.load()/write() equivalents, without this.super():
	   own-property overrides are not part of the prototype chain, so
	   LuCI.Class super() resolution cannot find them */
	o.load = function(section_id) {
		return listToText(this.map.data.get(
			this.uciconfig ?? this.section.uciconfig ?? this.map.config,
			this.ucisection ?? section_id,
			this.ucioption ?? this.option));
	};

	o.write = function(section_id, value) {
		const lines = textToList(value);

		if (!lines.length)
			return this.map.data.unset(this.map.config, section_id, this.ucioption ?? this.option);

		return this.map.data.set(this.map.config, section_id, this.ucioption ?? this.option, lines);
	};

	const check = CHECKS[kind];

	if (check)
		o.validate = function(section_id, value) {
			for (const line of textToList(value))
				if (!check.fn(line)) {
					return _('Строка «%s» не распознана: ожидается %s.').format(line, check.hint);
				}

			return true;
		};

	o.rows = opts.rows || 6;

	if (opts.placeholder)
		o.placeholder = opts.placeholder;

	return o;
}


function isTenantEnabled(t) {
	return t.enabled != null ? (t.enabled != '0') : true;
}

function tenantNetworks(t) {
	return L.toArray(uci.get('fencebreaker', t['.name'], 'networks'));
}

/*
	UCI network names (interface sections) that reference the given device
	via their "device" option or "ports" list.
*/
function deviceUciNetworks(name) {
	return uci.sections('network', 'interface').map(function(net) {
		if (net['.name'] == 'loopback')
			return null;

		const device = uci.get('network', net['.name'], 'device');
		const ports = L.toArray(uci.get('network', net['.name'], 'ports'));

		return (device == name || ports.indexOf(name) != -1) ? net['.name'] : null;
	}).filter(function(n) {
		return n != null;
	});
}

/*
	Find networks assigned to more than one enabled tenant; used to warn
	about ambiguous routing before the configuration is applied.
*/
function findNetworkOverlaps() {
	const nets = {};
	const owners = {};
	const overlaps = {};

	uci.sections('fencebreaker', 'tenant').forEach(function(t) {
		if (!isTenantEnabled(t))
			return;

		tenantNetworks(t).forEach(function(n) {
			if (!n)
				return;

			if (!nets[n])
				nets[n] = [];

			if (nets[n].indexOf(t.name || t.id || t['.name']) == -1)
				nets[n].push(t.name || t.id || t['.name']);

			owners[n] = true;
		});
	});

	for (const net in nets)
		if (nets[net].length > 1)
			overlaps[net] = nets[net];

	return overlaps;
}

function reportNetworkOverlaps(existingNode) {
	const overlaps = findNetworkOverlaps();
	const items = Object.keys(overlaps).map(function(net) {
		return E('li', {}, '%s — %s'.format(net, overlaps[net].join(', ')));
	});

	if (existingNode) {
		existingNode.classList.add('fade-out');
	}

	if (items.length) {
		const content = E('div', {}, [
			E('p', {}, _('Внимание: следующие сети назначены нескольким включённым тенантам одновременно. Трафик таких сетей будет обработан тенантом с наивысшим приоритетом (меньшее значение priority).')),
			E('ul', {}, items)
		]);

		return ui.addNotification(null, content, 'warning');
	}

	return null;
}

return view.extend({
	load: function() {
		return Promise.all([
			uci.load('fencebreaker'),
			uci.load('network'),
			L.resolveDefault(getDeviceStatus(), {})
		]);
	},

	render: function(data) {
		/* load() resolves to [uci, uci, device status]; render() receives
		   the whole array as one argument */
		const devstat = (Array.isArray(data) && typeof(data[2]) == 'object') ? data[2] : {};
		let m, s, o;

		m = new form.Map('fencebreaker', _('fencebreaker'),
			_('Тенант определяет, какой трафик (сети, хосты, DNS-ресурсы) направляется через выбранный канал: интерфейс или VLESS. Настройки перечитываются демоном при применении конфигурации.'));

		/*
			tenants
		*/
		s = m.section(form.GridSection, 'tenant', _('Тенанты'));
		s.addremove = true;
		s.anonymous = true;
		s.sortable = true;
		s.nodescriptions = true;
		s.sectiontitle = function(section_id) {
			const t = uci.get('fencebreaker', section_id, 'name');
			return t || section_id;
		};

		s.tab('general', _('Основное'));
		s.tab('channel', _('Канал'));
		s.tab('resources', _('Ресурсы'));
		s.tab('clients', _('Клиенты'));
		s.tab('dns', _('DNS'));

		/* columns shown in the table (and at the top of the edit modal) */

		o = s.option(form.Value, 'name', _('Название'));
		o.rmempty = false;
		o.placeholder = _('Например: Гости');

		o = s.option(form.Flag, 'enabled', _('Включён'));
		o.default = o.enabled;
		o.rmempty = false;

		o = s.option(form.ListValue, 'routing_mode', _('Режим маршрутизации'));
		o.value('select', _('select — маршрутизировать только указанные ресурсы'));
		o.value('invert', _('invert — маршрутизировать всё, кроме указанных исключений'));
		o.default = 'select';
		o.rmempty = false;

		/* Основное */

		o = s.taboption('general', form.Value, 'id', _('Идентификатор'));
		o.readonly = true;
		o.rmempty = false;

		o = s.taboption('general', form.Value, 'priority', _('Приоритет'),
			_('Меньшее число — важнее. Учитывается при конфликте полных хостов (full_hosts) между тенантами.'));
		o.datatype = 'uinteger';
		o.default = '100';
		o.placeholder = '100';

		/* Канал */

		o = s.taboption('channel', form.ListValue, 'egress_mode', _('Тип канала (egress)'),
			_('Сетевое устройство (интерфейс) или VLESS-туннель (sing-box).'));
		o.value('interface', _('Интерфейс'));
		o.value('vless', 'VLESS');
		o.default = 'interface';
		o.rmempty = false;

		o = s.taboption('channel', form.ListValue, 'egress_interface', _('Интерфейс'),
			_('Устройство (интерфейс), через которое уходит трафик тенанта: физический порт (eth1), мост или туннель (wg0, awg0). Список берётся из сетевых устройств роутера.'));

		const egressDevices = {};

		Object.keys(devstat || {}).forEach(function(name) {
			if (name != 'lo')
				egressDevices[name] = true;
		});

		/* a configured device may be temporarily absent: keep it selectable */
		uci.sections('fencebreaker', 'tenant').forEach(function(t) {
			const dev = uci.get('fencebreaker', t['.name'], 'egress_interface');

			if (dev)
				egressDevices[dev] = true;
		});

		Object.keys(egressDevices).sort().forEach(function(name) {
			const nets = deviceUciNetworks(name);

			o.value(name, nets.length ? '%s (%s)'.format(name, nets.join(', ')) : name);
		});

		o.depends('egress_mode', 'interface');

		o = s.taboption('channel', form.Value, 'vless_uri', _('VLESS-строка'),
			_('Ссылка вида vless://uuid@server:port?security=reality&sni=…&fp=…&pbk=…&sid=…&flow=… — вставляется целиком, демон разбирает её сам и строит конфиг sing-box. Поддерживается транспорт TCP (TLS/REALITY).'));
		o.depends('egress_mode', 'vless');
		o.placeholder = 'vless://uuid@server.org:443?security=reality&flow=xtls-rprx-vision';
		o.validate = function(section_id, value) {
			if (value == null || value == '')
				return true;

			if (!/^vless:\/\/[^@]+@[^:\/]+:\d{1,5}/i.test(String(value).trim()))
				return _('Ожидается ссылка вида vless://uuid@server:port?…');

			return true;
		};

		o = s.taboption('channel', form.ListValue, 'fallback', _('Fallback'));
		o.value('main', _('main — обычный режим отказа канала'));
		o.value('strict', _('strict — строгий режим отказа канала'));
		o.default = 'main';
		o.rmempty = false;

		/* Ресурсы */

		o = s.taboption('resources', form.TextValue, 'include_subnets', _('Включаемые подсети'),
			_('Подсети и адреса, трафик которых направляется через канал тенанта (режим select).'));
		wireList(o, 'cidr4', { placeholder: '1.2.3.0/24\n5.6.7.8/32' });

		o = s.taboption('resources', form.TextValue, 'exclude_subnets', _('Исключаемые подсети'),
			_('Подсети, которые никогда не направляются через канал тенанта (режим invert).'));
		wireList(o, 'cidr4', { placeholder: '5.6.7.8/32\n10.11.12.0/24' });

		o = s.taboption('resources', form.TextValue, 'include_domains', _('Включаемые домены'),
			_('Домены, адреса которых (из DNS-ответов) направляются через канал тенанта.'));
		wireList(o, 'hostname', { placeholder: 'example.com\napi.example.com' });

		o = s.taboption('resources', form.TextValue, 'exclude_domains', _('Исключаемые домены'));
		wireList(o, 'hostname', { placeholder: 'bank.example\ngov.example' });

		o = s.taboption('resources', form.MultiValue, 'include_geoip', _('Включаемые GeoIP-коды'),
			_('Коды стран ISO 3166-1 alpha-2, трафик которых направляется через канал тенанта (режим select).'));
		Object.keys(GEO_COUNTRIES).sort().forEach(function(code) {
			o.value(code, '%s — %s'.format(code, GEO_COUNTRIES[code]));
		});

		o = s.taboption('resources', form.MultiValue, 'exclude_geoip', _('Исключаемые GeoIP-коды'),
			_('Коды стран ISO 3166-1 alpha-2, которые никогда не направляются через канал тенанта (режим invert).'));
		Object.keys(GEO_COUNTRIES).sort().forEach(function(code) {
			o.value(code, '%s — %s'.format(code, GEO_COUNTRIES[code]));
		});

		/* DynamicList, not MultiValue: MultiValue crashes on empty choice
		   sets (ui.Dropdown Object.keys(undefined)) when no lists exist yet */
		o = s.taboption('resources', form.DynamicList, 'include_lists', _('Включаемые списки'),
			_('Имена списков (см. страницу «Списки»), содержимое которых направляется через канал тенанта.'));
		o.datatype = 'uciname';
		uci.sections('fencebreaker', 'list').forEach(function(l) {
			if (l.name)
				o.value(l.name, l.name);
		});

		o = s.taboption('resources', form.DynamicList, 'exclude_lists', _('Исключаемые списки'));
		o.datatype = 'uciname';
		uci.sections('fencebreaker', 'list').forEach(function(l) {
			if (l.name)
				o.value(l.name, l.name);
		});

		/* Клиенты */

		o = s.taboption('clients', form.DynamicList, 'networks', _('Клиентские сети'),
			_('UCI-сети и/или имена устройств-мостов (например, br-guest), клиенты которых обслуживаются этим тенантом. Сети разных включённых тенантов не должны пересекаться.'));
		o.datatype = 'or(uciname, cidr4)';
		o.placeholder = 'lan';
		uci.sections('network', 'interface').forEach(function(net) {
			if (net['.name'] != 'loopback')
				o.value(net['.name'], net['.name']);
		});
		uci.sections('network', 'device').forEach(function(dev) {
			if ((dev.name || '').indexOf('br-') == 0)
				o.value(dev.name, dev.name);
		});

		o = s.taboption('clients', form.DynamicList, 'excluded_hosts', _('Исключённые хосты'),
			_('Хосты из клиентских сетей, которые обслуживаются основным маршрутизатором, а не тенантом.'));
		o.datatype = 'ip4addr';
		o.placeholder = '192.168.1.50';

		o = s.taboption('clients', form.DynamicList, 'full_hosts', _('Полные хосты (full_hosts)'),
			_('Хосты, весь трафик которых (без DNS-обучения) направляется через канал тенанта.'));
		o.datatype = 'ip4addr';
		o.placeholder = '192.168.1.60';

		/* DNS */

		o = s.taboption('dns', form.DynamicList, 'dns_upstream', _('DNS-серверы тенанта'),
			_('Upstream-серверы, на которые перенаправляются запросы клиентов тенанта. Записей может быть несколько, поддерживаемые форматы:') +
			'<br>' + _('94.140.14.140 · [2a10:50c0::1:ff] · 94.140.14.140:53 — UDP (порт по умолчанию 53);') +
			'<br>' + _('udp://host[:53] · tcp://host[:53] · tls://host[:853] — UDP/TCP/DNS-over-TLS по IP или имени;') +
			'<br>' + _('https://host[:443][/dns-query] · h3://host[:443][/dns-query] — DNS-over-HTTPS (h3 — только HTTP/3);') +
			'<br>' + _('quic://host[:853] — DNS-over-QUIC;') +
			'<br>' + _('sdns://base64url — DNS Stamp (DNSCrypt/DoH).'));
		o.placeholder = '1.1.1.1';
		o.validate = function(section_id, value) {
			/* значение DynamicList приходит строкой при вводе и массивом при
			   сохранении — проверяем каждую запись */
			for (const addr of L.toArray(value)) {
				if (addr == null || addr == '')
					continue;

				if (!isDNSUpstreamAddr(addr))
					return _('Неподдерживаемый формат адреса: %s').format(addr);
			}

			return true;
		};

		o = s.taboption('dns', form.ListValue, 'upstream_policy', _('Выбор upstream-сервера'),
			_('Порядок выбора из списка серверов. Недоступный сервер исключается на 10 секунд, срок удваивается при повторных сбоях.'));
		o.value('sequential', _('последовательно'));
		o.value('random', _('случайный'));
		o.default = 'sequential';
		o.rmempty = false;

		o = s.taboption('dns', form.Flag, 'dns_hijack', _('Перехват DNS (порт 53)'),
			_('Перенаправлять DNS-запросы (TCP/UDP порт 53) клиентов тенанта на локальный CoreDNS.'));
		o.default = o.enabled;

		o = s.taboption('dns', form.Flag, 'dns_via_tunnel', _('DNS через туннель'),
			_('Направлять upstream-запросы DNS через канал тенанта (с его fwmark).'));
		o.default = o.enabled;

		o = s.taboption('dns', form.Flag, 'dns_keepalive', _('Продление адресов доменов'),
			_('Фоновый перезапрос доменов до истечения TTL, чтобы маршрутизация тенанта не прерывалась. Запросы идут через те же DNS-серверы тенанта.'));
		o.rmempty = true;

		/*
			New tenant sections get an auto-generated id "tenantN"
		*/
		const origHandleAdd = s.handleAdd;

		s.handleAdd = function(ev, name) {
			const before = {};

			uci.sections('fencebreaker', 'tenant').forEach(function(t) {
				before[t['.name']] = true;
			});

			const rv = origHandleAdd.call(this, ev, name);

			uci.sections('fencebreaker', 'tenant').forEach(L.bind(function(t) {
				if (before[t['.name']])
					return;

				const used = {};

				uci.sections('fencebreaker', 'tenant').forEach(function(x) {
					used[x.id || ''] = true;
				});

				let n = 1;

				while (used['tenant' + n])
					n++;

				this.map.data.set(this.map.config, t['.name'], 'id', 'tenant' + n);
			}, this));

			return rv;
		};

		/*
			DNS rules
		*/
		const ruleSection = m.section(form.GridSection, 'dnsrule', _('DNS-правила'),
			_('Правила CoreDNS-плагина: точные A-записи (record) и пересылка доменных зон (forward) для конкретных тенантов.'));

		ruleSection.addremove = true;
		ruleSection.anonymous = true;
		ruleSection.sortable = true;
		ruleSection.nodescriptions = true;

		ruleSection.tab('general', _('Правило'));

		o = ruleSection.taboption('general', form.Value, 'tenant', _('Тенант'),
			_('Идентификатор тенанта (id).'));
		uci.sections('fencebreaker', 'tenant').forEach(function(t) {
			if (t.id)
				o.value(t.id, t.name ? '%s (%s)'.format(t.id, t.name) : t.id);
		});
		o.placeholder = 'tenant1';

		o = ruleSection.taboption('general', form.ListValue, 'type', _('Тип'));
		o.value('record', _('record — точная A-запись'));
		o.value('forward', _('forward — пересылка доменной зоны'));
		o.default = 'record';
		o.rmempty = false;

		o = ruleSection.taboption('general', form.Value, 'domain', _('Домен'),
			_('Для record — точное имя (api.corp); для forward — суффикс зоны (corp.example), поддомены наследуются.'));
		o.datatype = 'hostname';
		o.rmempty = false;

		o = ruleSection.taboption('general', form.DynamicList, 'target', _('Назначение'),
			_('Для record — IP-адреса (10.0.0.5); для forward — серверы в виде «ip[:port]» (10.10.0.1:53).'));
		o.datatype = 'or(ip4addr, ipaddrport)';
		o.placeholder = '10.0.0.5';

		/*
			GridSection renders every option as a table column unless it is
			modalonly: keep both grids narrow, tabbed options live in the
			edit modal only (name/enabled/routing_mode stay).
		*/
		[ s, ruleSection ].forEach(function(sec) {
			Object.keys(sec.tabs || {}).forEach(function(t) {
				(sec.tabs[t].children || []).forEach(function(o) {
					o.modalonly = true;
				});
			});
		});

		/*
			Warn about networks shared by several enabled tenants on save
		*/
		let overlapMsg = null;

		const origSave = m.save.bind(m);

		m.save = function(cb, silent) {
			return origSave(cb, silent).then(function() {
				overlapMsg = reportNetworkOverlaps(overlapMsg);
			});
		};

		return m.render();
	},

});
