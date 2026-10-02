#!/usr/bin/env node
// FastClaw xiaoai channel plugin — bridges the Songloft miot plugin to the
// FastClaw agent loop:
//
//   speaker talk → XiaoAI cloud → Songloft conversation monitor (dedup,
//   baseline) → POST /webhook here → message.inbound → agent turn
//   agent reply → channel.send → POST /mina/tts on Songloft → speaker TTS
//
// Zero npm dependencies (Node >= 18 built-ins only). All logging goes to
// stderr; FastClaw forwards it as "plugin stderr" lines.

'use strict';

const http = require('http');
const readline = require('readline');

const CHANNEL = 'xiaoai';
const MIOT_API = '/api/v1/jsplugin/miot'; // entryPath=miot behind Songloft's host JWT

const DEFAULTS = {
	listenHost: '0.0.0.0',
	listenPort: '32100',
	maxChars: '400',
	ignoreKeywords:
		'下一首,上一首,暂停,继续播放,停止播放,单曲循环,循环播放,随机播放,顺序播放',
};

const cfg = { ...DEFAULTS };
const log = (...a) => console.error(`[${CHANNEL}]`, ...a);

// ---- state ----
const devices = new Map(); // device_id → { account_id, device_name, lastTs }
let httpServer = null;
let shuttingDown = false;

// ---- JSON-RPC over stdio ----
function send(obj) {
	process.stdout.write(JSON.stringify(obj) + '\n');
}
function reply(id, result) {
	send({ jsonrpc: '2.0', id, result: result || {} });
}
function replyErr(id, message) {
	send({ jsonrpc: '2.0', id, error: { code: -32000, message } });
}
function notify(method, params) {
	send({ jsonrpc: '2.0', method, params });
}

// ---- Songloft API client (login / refresh / retry once on 401) ----
const tokens = { access: '', refresh: '' };

function songloftBase() {
	return String(cfg.songloftUrl || '').replace(/\/+$/, '');
}

async function songloftLogin() {
	const res = await fetch(`${songloftBase()}/api/v1/auth/login`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ username: cfg.songloftUsername, password: cfg.songloftPassword }),
	});
	if (!res.ok) throw new Error(`songloft login http ${res.status}`);
	const j = await res.json();
	if (!j.access_token) throw new Error('songloft login: no access_token in response');
	tokens.access = j.access_token;
	tokens.refresh = j.refresh_token || tokens.refresh;
}

async function songloftApi(path, opts, retried) {
	if (!tokens.access) await songloftLogin();
	const res = await fetch(`${songloftBase()}${path}`, {
		method: (opts && opts.method) || 'GET',
		headers: {
			'Content-Type': 'application/json',
			Authorization: `Bearer ${tokens.access}`,
		},
		body: opts && opts.body,
	});
	if (res.status === 401 && !retried) {
		// Access token expired: refresh, fall back to a fresh login, retry once.
		try {
			const r = await fetch(`${songloftBase()}/api/v1/auth/refresh`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ refresh_token: tokens.refresh }),
			});
			const j = await r.json().catch(() => ({}));
			if (j.access_token) {
				tokens.access = j.access_token;
				tokens.refresh = j.refresh_token || tokens.refresh;
			} else {
				tokens.access = '';
				await songloftLogin();
			}
		} catch {
			tokens.access = '';
			await songloftLogin();
		}
		return songloftApi(path, opts, true);
	}
	return res;
}

// ---- webhook registration (idempotent, retried until Songloft is reachable) ----
function webhookUrl() {
	const base = String(cfg.publicBaseUrl || '').replace(/\/+$/, '');
	return `${base}/webhook?token=${encodeURIComponent(cfg.webhookToken || '')}`;
}

async function ensureWebhook(attempt) {
	if (shuttingDown) return;
	try {
		const url = webhookUrl();
		const bareUrl = url.split('?')[0];
		const res = await songloftApi(`${MIOT_API}/conversation/webhooks`);
		if (!res.ok) throw new Error(`list http ${res.status}`);
		const j = await res.json();
		const existing = (j.data || []).some((w) => String(w.url || '').split('?')[0] === bareUrl);
		if (!existing) {
			const add = await songloftApi(`${MIOT_API}/conversation/webhooks`, {
				method: 'POST',
				body: JSON.stringify({ url, name: 'fluctio' }),
			});
			if (!add.ok) throw new Error(`add http ${add.status}`);
		}
		log(`webhook registered at ${bareUrl} (attempt ${attempt})`);
	} catch (e) {
		if (attempt >= 10) {
			log('webhook registration gave up after', attempt, 'attempts:', e.message);
			return;
		}
		log(`webhook register failed (attempt ${attempt}), retrying in 60s:`, e.message);
		setTimeout(() => ensureWebhook(attempt + 1), 60_000);
	}
}

// ---- outbound TTS (agent reply → speaker) ----

// Strip common markdown noise the TTS voice would read out loud (#, *, `,
// links, table pipes). Deliberately light: code blocks and exotic syntax are
// rare in IM replies and get mangled either way.
function ttsSpeakable(text) {
	return String(text || '')
		.replace(/```[\s\S]*?```/g, '（代码略）')
		.replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
		.replace(/https?:\/\/\S+/g, '链接')
		.replace(/[#*_>`~|]+/g, '')
		.replace(/\s{2,}/g, ' ')
		.trim();
}

async function tts(deviceId, text) {
	const dev = devices.get(deviceId);
	if (!dev) {
		throw new Error(
			`unknown device ${deviceId}: no webhook received from it yet (account_id unknown)`,
		);
	}
	let t = ttsSpeakable(text);
	if (!t) return { skipped: 'empty text' };
	const max = parseInt(cfg.maxChars, 10) || 400;
	if (t.length > max) t = `${t.slice(0, max)}……内容较长，已截断`;
	const res = await songloftApi(`${MIOT_API}/mina/tts`, {
		method: 'POST',
		body: JSON.stringify({ account_id: dev.account_id, device_id: deviceId, text: t }),
	});
	if (!res.ok) throw new Error(`tts http ${res.status}`);
	return { ok: true };
}

// ---- inbound webhook (speaker talk → agent) ----

// ConversationMessage.message.response.answer[]: `question` /
// `intention.query` carry the user's ASR text; `content` is XiaoAI's own
// reply. Collect only the user-side fields.
function extractUserText(cm) {
	const answers =
		(cm.message && cm.message.response && cm.message.response.answer) || [];
	const parts = [];
	for (const a of answers) {
		const q = String(
			a.question || (a.intention && a.intention.query) || '',
		).trim();
		if (q) parts.push(q);
	}
	return parts.join(' ').trim();
}

// Exact-match only: player keywords are short commands on their own; prefix
// matching would swallow normal sentences like "暂停一下,先听我说".
function isIgnored(text) {
	return cfg.ignoreKeywords
		.split(',')
		.map((k) => k.trim())
		.filter(Boolean)
		.includes(text);
}

function onConversationPayload(payload) {
	for (const cm of payload.messages || []) {
		if (!cm || !cm.device_id) continue;
		const ts = (cm.message && cm.message.timestamp_ms) || 0;
		let st = devices.get(cm.device_id);
		if (!st) {
			st = { account_id: '', device_name: '', lastTs: 0 };
			devices.set(cm.device_id, st);
		}
		st.account_id = cm.account_id || st.account_id;
		st.device_name = cm.device_name || st.device_name;
		if (ts && ts <= st.lastTs) continue; // stale replay
		st.lastTs = ts;
		const text = extractUserText(cm);
		if (!text) continue; // XiaoAI's own reply / no user speech
		if (isIgnored(text)) continue; // Songloft's player voice commands
		log(`inbound from ${st.device_name || cm.device_id}: ${text.slice(0, 60)}`);
		notify('message.inbound', {
			channel: CHANNEL,
			chatId: cm.device_id,
			userId: cm.account_id || '',
			text,
			senderName: st.device_name || cm.device_id,
		});
	}
}

// ---- HTTP listener for Songloft's webhook pushes ----
function startListener() {
	httpServer = http.createServer((req, res) => {
		const u = new URL(req.url, 'http://localhost');
		if (req.method === 'POST' && u.pathname === '/webhook') {
			const token =
				u.searchParams.get('token') || req.headers['x-webhook-token'] || '';
			if (!cfg.webhookToken || token !== cfg.webhookToken) {
				res.writeHead(401).end();
				return;
			}
			let body = '';
			req.on('data', (c) => {
				body += c;
				if (body.length > 1 << 20) req.destroy(); // 1MB guard
			});
			req.on('end', () => {
				try {
					onConversationPayload(JSON.parse(body || '{}'));
					res.writeHead(204).end();
				} catch (e) {
					log('webhook parse error:', e.message);
					res.writeHead(400).end();
				}
			});
			return;
		}
		res.writeHead(404).end();
	});
	httpServer.on('error', (e) => log('listener error:', e.message));
	httpServer.listen(
		parseInt(cfg.listenPort, 10) || 32100,
		cfg.listenHost || '0.0.0.0',
		() => log(`webhook listener on ${cfg.listenHost || '0.0.0.0'}:${cfg.listenPort}`),
	);
}

// ---- JSON-RPC request handling (stdin, newline-delimited) ----
const rl = readline.createInterface({ input: process.stdin });
rl.on('line', (line) => {
	line = line.trim();
	if (!line) return;
	let msg;
	try {
		msg = JSON.parse(line);
	} catch {
		log('unparseable stdin line');
		return;
	}
	if (msg.id === undefined || msg.id === null) return; // no inbound notifications expected

	if (msg.method === 'initialize') {
		Object.assign(cfg, (msg.params && msg.params.config) || {});
		// Restore defaults for keys the config left empty.
		for (const [k, v] of Object.entries(DEFAULTS)) {
			if (cfg[k] === '' || cfg[k] === undefined) cfg[k] = v;
		}
		for (const req of ['songloftUrl', 'songloftUsername', 'songloftPassword', 'publicBaseUrl', 'webhookToken']) {
			if (!cfg[req]) log(`config "${req}" is MISSING — set it in the plugin config; webhook registration and TTS will fail until then`);
		}
		startListener();
		ensureWebhook(1); // async, retried internally; must not block initialize
		reply(msg.id, {});
		return;
	}
	if (msg.method === 'channel.send') {
		const { chatId, text } = msg.params || {};
		if (!chatId) {
			replyErr(msg.id, 'channel.send: chatId is required');
			return;
		}
		tts(chatId, text)
			.then((r) => reply(msg.id, r))
			.catch((e) => replyErr(msg.id, `tts: ${e.message}`));
		return;
	}
	if (msg.method === 'shutdown') {
		shuttingDown = true;
		reply(msg.id, {});
		setTimeout(() => process.exit(0), 50);
		return;
	}
	replyErr(msg.id, `unknown method ${msg.method}`);
});
rl.on('close', () => {
	// stdin closed = parent gateway is gone.
	if (httpServer) httpServer.close();
	process.exit(0);
});
