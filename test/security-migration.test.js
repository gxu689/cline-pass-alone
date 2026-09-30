import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import { spawn, spawnSync } from 'node:child_process';
import { once } from 'node:events';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';

async function freePort() {
  const server = http.createServer();
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  const port = server.address().port;
  await new Promise((resolve) => server.close(resolve));
  return port;
}

test('损坏的配置文件阻止启动，并保留原始内容', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cline-invalid-config-'));
  const file = path.join(dir, 'config.json');
  fs.writeFileSync(file, '{"accounts":');
  try {
    const child = spawnSync(process.execPath, ['server.js'], {
      cwd: path.resolve('.'), env: { ...process.env, DATA_DIR: dir }, timeout: 5000,
    });
    assert.notEqual(child.status, 0);
    assert.equal(fs.readFileSync(file, 'utf8'), '{"accounts":');
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

test('旧配置升级生成独立管理密钥，禁用账号后不再使用旧 key', { timeout: 10000 }, async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cline-legacy-config-'));
  const port = await freePort();
  let upstreamCalls = 0;
  const upstream = http.createServer((req, res) => {
    upstreamCalls++;
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ choices: [{ message: { content: 'unexpected' } }] }));
  });
  upstream.listen(0, '127.0.0.1');
  await once(upstream, 'listening');
  const configPath = path.join(dir, 'config.json');
  fs.writeFileSync(configPath, JSON.stringify({ port, upstreamBase: `http://127.0.0.1:${upstream.address().port}`, apiKey: 'legacy-upstream-key', proxyKey: 'existing-client-key', accounts: [], knownModels: ['cline-pass/test'] }));
  const child = spawn(process.execPath, ['server.js'], { cwd: path.resolve('.'), env: { ...process.env, DATA_DIR: dir, BIND_HOST: '127.0.0.1' }, stdio: 'ignore' });
  const base = `http://127.0.0.1:${port}`;
  try {
    let ready = false;
    for (let i = 0; i < 100; i++) {
      if (child.exitCode !== null) throw new Error('proxy exited before ready');
      try { ready = (await fetch(`${base}/api/meta`)).ok; } catch {}
      if (ready) break;
      await new Promise((resolve) => setTimeout(resolve, 25));
    }
    assert.ok(ready);
    const migrated = JSON.parse(fs.readFileSync(configPath, 'utf8'));
    assert.equal(migrated.proxyKey, 'existing-client-key');
    assert.ok(migrated.adminKey && migrated.adminKey !== migrated.proxyKey);
    assert.equal(migrated.apiKey, undefined);
    assert.equal(migrated.accounts[0].key, 'legacy-upstream-key');
    assert.equal(fs.statSync(configPath).mode & 0o777, 0o600);

    const adminHeaders = { 'X-Admin-Key': migrated.adminKey, 'Content-Type': 'application/json' };
    const stale = await fetch(`${base}/api/accounts`, { method: 'POST', headers: adminHeaders, body: JSON.stringify({ accounts: [{ id: 'stale-id', name: '旧页面', key: '••••••••' }] }) });
    assert.equal(stale.status, 409);
    assert.equal(JSON.parse(fs.readFileSync(configPath, 'utf8')).accounts[0].key, 'legacy-upstream-key');

    const id = crypto.createHash('sha256').update('legacy-upstream-key').digest('hex');
    const disabled = await fetch(`${base}/api/accounts`, { method: 'POST', headers: adminHeaders, body: JSON.stringify({ accounts: [{ id, name: '默认账号', key: '••••••••', enabled: false }], mode: 'single' }) });
    assert.equal(disabled.status, 200);
    const chat = await fetch(`${base}/v1/chat/completions`, { method: 'POST', headers: { Authorization: 'Bearer existing-client-key', 'Content-Type': 'application/json' }, body: JSON.stringify({ model: 'cline-pass/test', messages: [{ role: 'user', content: 'hi' }] }) });
    assert.equal(chat.status, 503);
    assert.equal(upstreamCalls, 0);
  } finally {
    child.kill();
    await new Promise((resolve) => upstream.close(resolve));
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
