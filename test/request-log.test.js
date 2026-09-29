import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { RequestLogStore, LOG_RETENTION_MS } from '../request-log.js';

test('日志保留近两天，支持服务端筛选、分页和统计', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cline-logs-'));
  const file = path.join(dir, 'request-logs.jsonl');
  const now = Date.UTC(2026, 8, 29, 12);
  try {
    const store = new RequestLogStore(file, [
      { ts: now - LOG_RETENTION_MS - 1, model: 'expired', status: 200 },
      { ts: now - 3600e3, model: 'cline-pass/a', status: 200, account: 'A', accountId: 'id-a', usage: { total_tokens: 10 } },
      { ts: now - 1800e3, model: 'cline-pass/b', status: 429, account: 'B', accountId: 'id-b', error: 'rate limited' },
    ], now);
    store.append({ model: 'cline-pass/a', status: 200, account: 'A', accountId: 'id-a', stream: true, endpoint: '/v1/chat/completions', usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5, cost: 0.01 }, trace: [{ status: 429 }, { status: 200 }] }, now);
    assert.equal(store.rows.length, 3);
    const page1 = store.query({ pageSize: 1 }, now);
    assert.equal(page1.total, 3);
    assert.equal(page1.pages, 3);
    assert.equal(page1.history[0].model, 'cline-pass/a');
    assert.equal(store.query({ page: 2, pageSize: 1 }, now).history[0].status, 429);
    assert.equal(store.query({ status: 'error' }, now).total, 1);
    assert.equal(store.query({ status: '429', q: 'rate' }, now).total, 1);
    assert.equal(store.query({ model: 'cline-pass/a', accountId: 'id-a' }, now).total, 2);
    assert.equal(store.query({ stream: 'stream', endpoint: '/v1/chat/completions' }, now).total, 1);
    assert.equal(store.query({ stream: 'sync' }, now).total, 2);
    assert.deepEqual(page1.endpoints, ['/v1/chat/completions']);
    assert.equal(store.query({ from: now - 1000, to: now }, now).total, 1);
    assert.equal(page1.counts.retries, 1);
    const stats = store.stats(0, now);
    assert.equal(stats.today.requests, 3);
    assert.equal(stats.today.errors, 1);
    assert.equal(stats.today.tokens, 15);
    assert.equal(stats.rpm, 1);
    assert.equal(stats.today.cost, 0.01);
    const reloaded = new RequestLogStore(file, [], now);
    assert.equal(fs.statSync(file).mode & 0o777, 0o600);
    assert.equal(reloaded.rows.length, 3);
    assert.equal(reloaded.stats(0, now).today.tokens, 15);
    assert.equal(reloaded.stats(0, now).today.cost, 0.01);
    assert.equal(reloaded.query({}, now).counts.retries, 1);
    assert.equal(reloaded.query({ stream: 'stream' }, now).history[0].totalTokens, 5);
    store.prune(now + LOG_RETENTION_MS + 60001);
    assert.equal(store.rows.length, 0);
    const resumedMigration = new RequestLogStore(file, [{ ts: now, model: 'legacy', status: 200 }], now);
    assert.equal(resumedMigration.query({}, now).total, 1);
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});
