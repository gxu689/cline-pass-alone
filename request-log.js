import fs from 'node:fs';

export const LOG_RETENTION_MS = 48 * 60 * 60 * 1000;
const MAX_ROWS_PER_PAGE = 100;

const number = (...values) => {
  for (const value of values) {
    const n = Number(value);
    if (value !== null && value !== undefined && value !== '' && Number.isFinite(n)) return n;
  }
  return null;
};
const short = (value, limit = 200) => String(value ?? '').slice(0, limit);

export function normalizeLog(row) {
  const usage = row.usage || {};
  const inputTokens = number(row.inputTokens, usage.prompt_tokens, usage.input_tokens, usage.inputTokens) || 0;
  const outputTokens = number(row.outputTokens, usage.completion_tokens, usage.output_tokens, usage.outputTokens) || 0;
  const totalTokens = number(row.totalTokens, usage.total_tokens, usage.totalTokens) ?? inputTokens + outputTokens;
  const cost = number(row.cost, usage.total_cost, usage.totalCost, usage.cost, usage.amount);
  return {
    ts: Number(row.ts) || Date.now(),
    model: short(row.model), status: Number(row.status) || (row.error ? 502 : 200),
    provider: short(row.provider, 80), canonical: short(row.canonical),
    account: short(row.account, 80), accountId: short(row.accountId, 64),
    ms: Math.max(0, Number(row.ms) || 0), stream: !!row.stream,
    inputTokens, outputTokens, totalTokens, cost,
    attempts: number(row.attempts) ?? (Array.isArray(row.trace) ? row.trace.length : 1),
    trace: (Array.isArray(row.trace) ? row.trace : []).slice(0, 20).map((t) => ({ upstream: short(t.upstream, 80), account: short(t.account, 80), status: Number(t.status) || 0, ms: Number(t.ms) || 0, note: short(t.note, 160) })),
    error: short(row.error, 300), endpoint: short(row.endpoint || '/v1/chat/completions', 100),
    ip: short(row.ip, 64), userAgent: short(row.userAgent, 200),
  };
}

export class RequestLogStore {
  constructor(file, legacy = [], now = Date.now()) {
    this.file = file;
    const exists = fs.existsSync(file);
    const persisted = exists ? fs.readFileSync(file, 'utf8').split('\n').filter(Boolean).map((line) => {
      try { return JSON.parse(line); } catch { return null; }
    }).filter(Boolean) : [];
    const rows = persisted.length ? persisted : legacy;
    this.rows = rows.filter((r) => Number(r.ts) >= now - LOG_RETENTION_MS && Number(r.ts) <= now + 60000).map(normalizeLog);
    this.rows.sort((a, b) => a.ts - b.ts);
    this.lastPrune = now;
    this.flush();
  }

  flush() {
    const temp = `${this.file}.tmp`;
    fs.writeFileSync(temp, this.rows.map((r) => JSON.stringify(r)).join('\n') + (this.rows.length ? '\n' : ''), { mode: 0o600 });
    fs.renameSync(temp, this.file);
    fs.chmodSync(this.file, 0o600);
  }

  prune(now = Date.now()) {
    if (now - this.lastPrune < 60000) return;
    this.lastPrune = now;
    const before = this.rows.length;
    this.rows = this.rows.filter((r) => r.ts >= now - LOG_RETENTION_MS);
    if (this.rows.length !== before) this.flush();
  }

  append(row, now = Date.now()) {
    this.prune(now);
    const item = normalizeLog({ ...row, ts: now });
    this.rows.push(item);
    fs.appendFileSync(this.file, `${JSON.stringify(item)}\n`, { mode: 0o600 });
    return item;
  }

  query({ from, to, status = 'all', model = '', accountId = '', endpoint = '', stream = 'all', q = '', page = 1, pageSize = 50 } = {}, now = Date.now()) {
    this.prune(now);
    const lower = Math.max(now - LOG_RETENTION_MS, Number.isFinite(Number(from)) ? Number(from) : now - LOG_RETENTION_MS);
    const upper = Math.min(now + 60000, Number.isFinite(Number(to)) ? Number(to) : now + 60000);
    const search = short(q, 120).toLowerCase();
    const base = this.rows.filter((r) => r.ts >= lower && r.ts <= upper && (!model || r.model === model) && (!accountId || r.accountId === accountId) && (!endpoint || r.endpoint === endpoint) && (stream === 'all' || r.stream === (stream === 'stream')) && (!search || [r.model, r.account, r.provider, r.canonical, r.error, r.ip, r.userAgent, r.endpoint, String(r.status)].some((v) => v.toLowerCase().includes(search))));
    const counts = { all: base.length, success: 0, error: 0, '4xx': 0, '5xx': 0, '401': 0, '429': 0, '499': 0, retries: 0 };
    for (const r of base) {
      if (r.status >= 200 && r.status < 400) counts.success++;
      else counts.error++;
      if (r.status >= 400 && r.status < 500) counts['4xx']++;
      if (r.status >= 500) counts['5xx']++;
      if (r.status === 401) counts['401']++;
      if (r.status === 429) counts['429']++;
      if (r.status === 499) counts['499']++;
      if (r.attempts > 1) counts.retries++;
    }
    const matches = (r) => status === 'all' || (status === 'success' && r.status >= 200 && r.status < 400) || (status === 'error' && r.status >= 400) || (status === '4xx' && r.status >= 400 && r.status < 500) || (status === '5xx' && r.status >= 500) || String(r.status) === status;
    const filtered = base.filter(matches).reverse();
    const size = Math.min(MAX_ROWS_PER_PAGE, Math.max(1, Number(pageSize) || 50));
    const requestedPage = Math.max(1, Math.trunc(Number(page) || 1));
    const pages = Math.max(1, Math.ceil(filtered.length / size));
    const currentPage = Math.min(requestedPage, pages);
    return { history: filtered.slice((currentPage - 1) * size, currentPage * size), total: filtered.length, page: currentPage, pageSize: size, pages, counts,
      models: [...new Set(this.rows.map((r) => r.model).filter(Boolean))].sort(),
      accounts: [...new Map(this.rows.filter((r) => r.accountId).map((r) => [r.accountId, { id: r.accountId, name: r.account }])).values()].sort((a, b) => a.name.localeCompare(b.name)),
      endpoints: [...new Set(this.rows.map((r) => r.endpoint).filter(Boolean))].sort(),
      retentionHours: 48 };
  }

  stats(tzOffsetMinutes = 0, now = Date.now()) {
    this.prune(now);
    const offset = Math.max(-840, Math.min(840, Number(tzOffsetMinutes) || 0)) * 60000;
    const todayStart = Math.floor((now + offset) / 86400000) * 86400000 - offset;
    const today = this.rows.filter((r) => r.ts >= todayStart);
    const recent = this.rows.filter((r) => r.ts >= now - 60000);
    const totals = (rows) => rows.reduce((a, r) => {
      a.requests++; a.success += r.status >= 200 && r.status < 400 ? 1 : 0;
      a.errors += r.status >= 400 ? 1 : 0;
      a.inputTokens += r.inputTokens; a.outputTokens += r.outputTokens; a.tokens += r.totalTokens;
      a.ms += r.ms; a.stream += r.stream ? 1 : 0; a.retries += r.attempts > 1 ? 1 : 0;
      if (r.cost != null) { a.cost += r.cost; a.costSamples++; }
      return a;
    }, { requests: 0, success: 0, errors: 0, inputTokens: 0, outputTokens: 0, tokens: 0, ms: 0, stream: 0, retries: 0, cost: 0, costSamples: 0 });
    const day = totals(today);
    const twoDays = totals(this.rows);
    const minute = totals(recent);
    const byModel = new Map();
    for (const row of today) {
      if (!byModel.has(row.model)) byModel.set(row.model, []);
      byModel.get(row.model).push(row);
    }
    const models = [...byModel].map(([model, rows]) => ({ model, ...totals(rows) })).sort((a, b) => b.requests - a.requests);
    return { today: day, twoDays, rpm: minute.requests, tpm: minute.tokens, errorRate: day.requests ? day.errors / day.requests * 100 : 0, averageMs: day.requests ? Math.round(day.ms / day.requests) : 0, models, updatedAt: now, retentionHours: 48 };
  }
}
