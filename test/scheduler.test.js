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

test('全局调度、429 换号、传输策略和流式首包重试', { timeout: 20000 }, async () => {
  const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'cline-scheduler-'));
  const proxyPort = await freePort();
  const events = [];
  const routeEvents = [];
  const active = { total: 0, a: 0, b: 0, maxTotal: 0, maxA: 0, maxB: 0 };
  let behavior = 'delay';
  let stickyFailures = 0;
  const upstream = http.createServer(async (req, res) => {
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
      res.end('data: {"choices":[{"delta":{"content":"hi"}}]}\n\ndata: [DONE]\n\n');
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
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ choices: [{ message: { content: 'ok' } }] }));
  });
  upstream.listen(0, '127.0.0.1');
  await once(upstream, 'listening');
  const upstreamPort = upstream.address().port;
  fs.writeFileSync(path.join(dataDir, 'config.json'), JSON.stringify({
    port: proxyPort, upstreamBase: `http://127.0.0.1:${upstreamPort}`,
    accounts: [{ name: 'A', key: 'a' }, { name: 'B', key: 'b' }],
    accountMode: 'roundrobin', accountConcurrency: 1, globalConcurrency: 2,
    globalRpm: 0, maxRetries: 0, knownModels: ['cline-pass/test'],
  }));
  const child = spawn(process.execPath, ['server.js'], { cwd: path.resolve('.'), env: { ...process.env, DATA_DIR: dataDir, BIND_HOST: '127.0.0.1' }, stdio: 'ignore' });
  const base = `http://127.0.0.1:${proxyPort}`;
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
    assert.ok((await Promise.all(Array.from({ length: 6 }, () => chat()))).every((r) => r.status === 200));
    assert.ok(active.maxTotal <= 2, `global concurrency ${active.maxTotal}`);
    assert.ok(active.maxA <= 1 && active.maxB <= 1, `account concurrency ${active.maxA}/${active.maxB}`);

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

    await settings({ maxRetries: 0 }); behavior = 'fallback'; routeEvents.length = 0;
    const fallback = await fetch(`${base}/api/test`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ model: 'cline-pass/test', upstreams: ['alpha', 'beta'] }) });
    const fallbackBody = await fallback.json();
    assert.equal(fallbackBody.ok, true);
    assert.deepEqual(routeEvents, ['alpha', 'beta']);
    assert.deepEqual(fallbackBody.trace.map((t) => t.status), [502, 200]);

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
