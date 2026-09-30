import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { once } from 'node:events';
import test from 'node:test';

async function freePort() {
  const server = http.createServer();
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  const port = server.address().port;
  await new Promise((resolve) => server.close(resolve));
  return port;
}

test('未配置上游账号时控制台仍可登录并展示页面', { timeout: 10000 }, async () => {
  const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'cline-console-'));
  const port = await freePort();
  fs.writeFileSync(path.join(dataDir, 'config.json'), JSON.stringify({ port, adminKey: 'console-admin-key', proxyKey: 'client-test-key', accounts: [] }));
  const metadataPath = path.join(dataDir, 'metadata.json');
  fs.writeFileSync(metadataPath, JSON.stringify({ models: { 'cline-pass/test': { lastProvider: 'known', account: 'old-name', trace: [{ status: 200 }] } }, history: [] }));
  const child = spawn(process.execPath, ['server.js'], { cwd: path.resolve('.'), env: { ...process.env, DATA_DIR: dataDir, BIND_HOST: '127.0.0.1' }, stdio: 'ignore' });
  const base = `http://127.0.0.1:${port}`;
  try {
    let ready = false;
    for (let i = 0; i < 100; i++) {
      if (child.exitCode !== null) throw new Error('proxy exited before ready');
      try { ready = (await fetch(`${base}/api/meta`)).ok; } catch {}
      if (ready) break;
      await new Promise((resolve) => setTimeout(resolve, 25));
    }
    assert.ok(ready, 'proxy did not start');
    assert.equal((await fetch(`${base}/api/models`)).status, 401);
    const headers = { 'X-Admin-Key': 'console-admin-key' };
    const modelsResponse = await fetch(`${base}/api/models`, { headers });
    assert.equal(modelsResponse.status, 200);
    assert.ok(Array.isArray((await modelsResponse.json()).subscription));
    assert.equal((await fetch(`${base}/api/security`, { headers })).status, 200);
    assert.equal((await fetch(`${base}/api/security`, { headers: { Authorization: 'Bearer client-test-key' } })).status, 401);
    assert.equal((await fetch(`${base}/api/security`, { method: 'POST', headers: { Authorization: 'Bearer client-test-key', 'Content-Type': 'application/json' }, body: JSON.stringify({ adminKey: 'hijacked' }) })).status, 401);
    assert.equal((await fetch(`${base}/api/security`, { method: 'POST', headers: { ...headers, 'Content-Type': 'application/json' }, body: JSON.stringify({ adminKey: 'client-test-key' }) })).status, 400);
    assert.equal((await fetch(`${base}/v1/models`, { headers })).status, 401);
    assert.equal((await fetch(`${base}/v1/models`, { headers: { Authorization: 'Bearer client-test-key' } })).status, 200);
    assert.equal((await fetch(`${base}/v1/chat/completions`, { method: 'POST', headers: { Authorization: 'Bearer client-test-key', 'Content-Type': 'application/json' }, body: JSON.stringify({ model: 'cline-pass/test', messages: [] }) })).status, 503);
    const rotated = await fetch(`${base}/api/security`, { method: 'POST', headers: { ...headers, 'Content-Type': 'application/json' }, body: JSON.stringify({ adminKey: 'new-console-admin-key', proxyKey: 'new-client-api-key' }) });
    assert.equal(rotated.status, 200);
    assert.equal((await fetch(`${base}/api/security`, { headers })).status, 401);
    assert.equal((await fetch(`${base}/api/security`, { headers: { 'X-Admin-Key': 'new-console-admin-key' } })).status, 200);
    assert.equal((await fetch(`${base}/v1/models`, { headers: { Authorization: 'Bearer client-test-key' } })).status, 401);
    assert.equal((await fetch(`${base}/v1/models`, { headers: { Authorization: 'Bearer new-client-api-key' } })).status, 200);
    const migratedModel = JSON.parse(fs.readFileSync(metadataPath, 'utf8')).models['cline-pass/test'];
    assert.equal(migratedModel.lastProvider, 'known');
    assert.equal(migratedModel.account, undefined);
    assert.equal(migratedModel.trace, undefined);
    const page = await (await fetch(base)).text();
    assert.match(page, /运行概览/);
    assert.match(page, /id="overviewAccounts"/);
    assert.match(page, /id="secAdminKey"/);
  } finally {
    child.kill();
    fs.rmSync(dataDir, { recursive: true, force: true });
  }
});

test('账户周期用量与全局调度、换号及流式重试', { timeout: 20000 }, async () => {
  const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'cline-scheduler-'));
  const proxyPort = await freePort();
  const events = [];
  const routeEvents = [];
  const active = { total: 0, a: 0, b: 0, maxTotal: 0, maxA: 0, maxB: 0 };
  let behavior = 'delay';
  let usageMode = 'periods';
  let stickyFailures = 0;
  const upstream = http.createServer(async (req, res) => {
    if (req.url === '/users/me') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ id: 'test-user' }));
      return;
    }
    if (req.url === '/users/test-user/usage') {
      res.writeHead(usageMode === 'balance' ? 401 : 200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(usageMode === 'balance' ? { error: 'unauthorized' } : { data: { spark: { used: 0, limit: 100 }, '7d': { used: 29, limit: 100 } } }));
      return;
    }
    if (req.url === '/users/test-user/balance') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ used: 29, limit: 100, remaining: 71 }));
      return;
    }
    if (req.url !== '/chat/completions') { res.writeHead(404).end(); return; }
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const request = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    const route = request.provider?.only?.[0] || request.providerOptions?.gateway?.only?.[0] || 'auto';
    routeEvents.push(route);
    const key = String(req.headers.authorization).split(' ')[1];
    events.push(key);
    active.total++; active[key]++;
    active.maxTotal = Math.max(active.maxTotal, active.total);
    active.maxA = Math.max(active.maxA, active.a);
    active.maxB = Math.max(active.maxB, active.b);
    const finish = () => { active.total--; active[key]--; };
    res.on('close', finish);
    if (behavior === 'delay') await new Promise((resolve) => setTimeout(resolve, 80));
    if (behavior === 'fallback' && route === 'alpha') {
      res.writeHead(502, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: { message: 'channel unavailable' } }));
      return;
    }
    if ((behavior === 'rate' || behavior === 'stream') && key === 'a') {
      res.writeHead(429, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: { message: 'rate limit' } }));
      return;
    }
    if ((behavior === 'transport' && key === 'a') || (behavior === 'sticky' && key === 'a' && stickyFailures++ === 0)) {
      res.destroy();
      return;
    }
    if (behavior === 'stream') {
      res.writeHead(200, { 'Content-Type': 'text/event-stream' });
      res.end('data: {"choices":[{"delta":{"content":"hi"}}]}\n\ndata: {"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}\n\ndata: [DONE]\n\n');
      return;
    }
    if (behavior === 'fragment' || behavior === 'late-error') {
      res.writeHead(200, { 'Content-Type': 'text/event-stream' });
      if (behavior === 'fragment') {
        res.write('da');
        setTimeout(() => res.end('ta: {"choices":[{"delta":{"content":"hi"}}]}\n\n'), 10);
      } else {
        res.write('data: {"choices":[{"delta":{"content":"hi"}}]}\n\n');
        setTimeout(() => res.end('data: {"error":{"message":"late"}}\n\n'), 10);
      }
      return;
    }
    if (behavior === 'hang-stream') {
      res.writeHead(200, { 'Content-Type': 'text/event-stream' });
      res.write('data: {"choices":[{"delta":{"content":"hi"}}]}\n\n');
      return;
    }
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ choices: [{ message: { content: 'ok' } }], usage: { prompt_tokens: 2, completion_tokens: 3, total_tokens: 5, cost: 0.001 } }));
  });
  upstream.listen(0, '127.0.0.1');
  await once(upstream, 'listening');
  const upstreamPort = upstream.address().port;
  fs.writeFileSync(path.join(dataDir, 'config.json'), JSON.stringify({
    port: proxyPort, upstreamBase: `http://127.0.0.1:${upstreamPort}`,
    adminKey: 'test-admin-key', proxyKey: 'test-client-key',
    accounts: [{ name: 'A', key: 'a' }, { name: 'B', key: 'b' }],
    accountMode: 'roundrobin', accountConcurrency: 1, globalConcurrency: 2,
    globalRpm: 0, maxRetries: 0, streamIdleTimeoutMs: 1000, knownModels: ['cline-pass/test'],
  }));
  const child = spawn(process.execPath, ['server.js'], { cwd: path.resolve('.'), env: { ...process.env, DATA_DIR: dataDir, BIND_HOST: '127.0.0.1' }, stdio: 'ignore' });
  const base = `http://127.0.0.1:${proxyPort}`;
  const fetch = (url, options = {}) => {
    const pathname = new URL(url).pathname;
    const auth = pathname.startsWith('/api/') && !pathname.startsWith('/api/v1/')
      ? { 'X-Admin-Key': 'test-admin-key' }
      : { Authorization: 'Bearer test-client-key' };
    return globalThis.fetch(url, { ...options, headers: { ...options.headers, ...auth } });
  };
  const settings = async (extra = {}) => {
    const res = await fetch(`${base}/api/accounts`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({
      accounts: [{ name: 'A', key: '••••••••' }, { name: 'B', key: '••••••••' }],
      mode: 'roundrobin', active: 0, accountConcurrency: 1, globalConcurrency: 2,
      globalRpm: 0, maxRetries: 2, rateLimitSwitchRetries: 1, retryIntervalMs: 0, transportRetryStrategy: 'switch', ...extra,
    }) });
    assert.equal(res.status, 200);
  };
  const chat = (stream = false, signal) => fetch(`${base}/v1/chat/completions`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ model: 'cline-pass/test', messages: [{ role: 'user', content: 'hi' }], stream }), signal });
  try {
    let ready = false;
    for (let i = 0; i < 100; i++) {
      if (child.exitCode !== null) throw new Error('proxy exited before ready');
      try { ready = (await fetch(`${base}/api/meta`)).ok; } catch {}
      if (ready) break;
      await new Promise((resolve) => setTimeout(resolve, 25));
    }
    assert.ok(ready, 'proxy did not start');
    const usage = await (await fetch(`${base}/api/accounts/usage?refresh=1`)).json();
    assert.equal(usage.usage[0].periods.find((p) => p.label === 'spark').percent, 0);
    assert.equal(usage.usage[0].periods.find((p) => p.label === '7d').percent, 29);
    const selectedUsage = await (await fetch(`${base}/api/accounts/usage?id=${usage.usage[0].id}&refresh=1`)).json();
    assert.equal(selectedUsage.usage.length, 1);
    assert.equal(selectedUsage.usage[0].id, usage.usage[0].id);
    usageMode = 'balance';
    const balance = await (await fetch(`${base}/api/accounts/usage?id=${usage.usage[0].id}&refresh=1`)).json();
    assert.equal(balance.usage[0].used, 29);
    assert.deepEqual(balance.usage[0].periods, []);
    usageMode = 'periods';
    assert.ok((await Promise.all(Array.from({ length: 6 }, () => chat()))).every((r) => r.status === 200));
    assert.ok(active.maxTotal <= 2, `global concurrency ${active.maxTotal}`);
    assert.ok(active.maxA <= 1 && active.maxB <= 1, `account concurrency ${active.maxA}/${active.maxB}`);
    const historyPage = await (await fetch(`${base}/api/history?status=success&pageSize=2&page=2`)).json();
    assert.equal(historyPage.total, 6);
    assert.equal(historyPage.pages, 3);
    assert.equal(historyPage.history.length, 2);
    const stats = await (await fetch(`${base}/api/stats?tzOffset=480`)).json();
    assert.ok(stats.today.requests >= 6);
    assert.ok(stats.today.tokens >= 30);
    const metadataPath = path.join(dataDir, 'metadata.json');
    assert.equal(fs.statSync(metadataPath).mode & 0o777, 0o600);
    const modelMeta = JSON.parse(fs.readFileSync(metadataPath, 'utf8')).models['cline-pass/test'];
    assert.equal(modelMeta.account, undefined);
    assert.equal(modelMeta.trace, undefined);
    assert.equal(modelMeta.usage, undefined);

    await settings(); behavior = 'rate'; events.length = 0;
    const rate = await chat();
    assert.equal(rate.status, 200);
    assert.deepEqual(events, ['a', 'b']);
    assert.equal(rate.headers.get('x-cline-attempts'), '2');
    await rate.body.cancel();

    await settings(); behavior = 'transport'; events.length = 0;
    const transport = await chat();
    assert.equal(transport.status, 200);
    assert.deepEqual(events, ['a', 'b']);
    await transport.body.cancel();

    await settings({ transportRetryStrategy: 'sticky' }); behavior = 'sticky'; stickyFailures = 0; events.length = 0;
    const sticky = await chat();
    assert.equal(sticky.status, 200);
    assert.deepEqual(events, ['a', 'a']);
    await sticky.body.cancel();

    await settings(); behavior = 'stream'; events.length = 0;
    const streamed = await chat(true);
    assert.equal(streamed.status, 200);
    assert.equal(streamed.headers.get('x-cline-attempts'), '2');
    assert.match(await streamed.text(), /"content":"hi"/);
    assert.deepEqual(events, ['a', 'b']);
    const streamLog = await (await fetch(`${base}/api/history?status=success&pageSize=1`)).json();
    assert.equal(streamLog.history[0].stream, true);
    assert.equal(streamLog.history[0].totalTokens, 6);
    const streamOnly = await (await fetch(`${base}/api/history?stream=stream&endpoint=%2Fv1%2Fchat%2Fcompletions`)).json();
    assert.equal(streamOnly.total, 1);
    assert.equal(streamOnly.history[0].stream, true);
    assert.equal((await fetch(`${base}/api/history?stream=invalid`)).status, 400);

    await settings(); behavior = 'fragment'; events.length = 0;
    const fragmented = await chat(true);
    assert.equal(fragmented.status, 200);
    assert.match(await fragmented.text(), /"content":"hi"/);
    assert.equal(events.length, 1, 'fragmented first SSE line must not trigger retry');

    await settings(); behavior = 'late-error'; events.length = 0;
    const lateError = await chat(true);
    assert.equal(lateError.status, 200);
    assert.match(await lateError.text(), /"message":"late"/);
    assert.equal(events.length, 1, 'SSE errors after the first event must not trigger retry');
    const lateErrorLog = await (await fetch(`${base}/api/history?pageSize=1`)).json();
    assert.equal(lateErrorLog.history[0].status, 502, 'late SSE error must not be logged as success');

    await settings(); behavior = 'hang-stream';
    const hung = await chat(true);
    assert.equal(hung.status, 200);
    await assert.rejects(hung.text());
    const timeoutLog = await (await fetch(`${base}/api/history?pageSize=1`)).json();
    assert.equal(timeoutLog.history[0].status, 504, 'idle stream must release its slot and be logged as timeout');
    assert.equal((await (await fetch(`${base}/api/accounts`)).json()).scheduling.active, 0);

    await settings({ maxRetries: 0 }); behavior = 'fallback'; routeEvents.length = 0;
    const fallback = await fetch(`${base}/api/test`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ model: 'cline-pass/test', upstreams: ['alpha', 'beta'] }) });
    const fallbackBody = await fallback.json();
    assert.equal(fallbackBody.ok, true);
    assert.deepEqual(routeEvents, ['alpha', 'beta']);
    assert.deepEqual(fallbackBody.trace.map((t) => t.status), [502, 200]);

    const excludeConfig = (exclude) => fetch(`${base}/api/config`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ perModel: { 'cline-pass/test': { upstreams: [], exclude } } }) });
    assert.equal((await excludeConfig(['alpha'])).status, 200);
    const beforeExcluded = routeEvents.length;
    const excluded = await chat();
    assert.equal(excluded.status, 409, 'unknown allowlist must fail closed');
    assert.equal(routeEvents.length, beforeExcluded, 'excluded request must not reach upstream');
    assert.equal((await excludeConfig([])).status, 200);

    await settings({ globalRpm: 2, maxRetries: 0 }); behavior = 'delay'; events.length = 0;
    await Promise.all([chat(), chat()]);
    const queued = chat();
    await new Promise((resolve) => setTimeout(resolve, 150));
    assert.equal(events.length, 2, 'RPM must hold the third request');
    const snapshot = await (await fetch(`${base}/api/accounts`)).json();
    assert.equal(snapshot.scheduling.queued, 1);
    const controller = new AbortController();
    const cancelled = chat(false, controller.signal).catch((e) => e);
    await new Promise((resolve) => setTimeout(resolve, 40));
    controller.abort();
    await cancelled;
    await new Promise((resolve) => setTimeout(resolve, 40));
    assert.equal((await (await fetch(`${base}/api/accounts`)).json()).scheduling.queued, 1, 'aborted request must leave the queue');
    await settings({ globalRpm: 0 });
    assert.equal((await queued).status, 200);
  } finally {
    child.kill();
    await new Promise((resolve) => upstream.close(resolve));
    fs.rmSync(dataDir, { recursive: true, force: true });
  }
});
