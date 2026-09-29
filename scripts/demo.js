// Isolated local demo. All accounts and usage below are synthetic; no real credentials are loaded.
import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cline-pass-demo-'));
const accountId = (key) => crypto.createHash('sha256').update(key).digest('hex');
const accounts = [
  { name: '示例账号 A', key: 'demo-account-a', enabled: true },
  { name: '示例账号 B', key: 'demo-account-b', enabled: true },
  { name: '示例账号 C', key: 'demo-account-c', enabled: true },
];
const models = ['cline-pass/glm-5.3', 'cline-pass/kimi-k3', 'cline-pass/qwen3.8-max'];
const upstream = http.createServer(async (req, res) => {
  const send = (status, value) => {
    res.writeHead(status, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(value));
  };
  if (req.url === '/models') return send(200, { data: models.map((id) => ({ id })) });
  if (req.url === '/users/me') return send(200, { id: 'demo-user' });
  if (req.url === '/users/demo-user/usage') return send(200, { data: { periods: { '5h': { used: 24, limit: 100 }, '7d': { used: 59, limit: 100 } } } });
  if (req.url === '/users/demo-user/balance') return send(200, { used: 59, limit: 100, remaining: 41 });
  if (req.url !== '/chat/completions') return send(404, { error: { message: 'demo endpoint not found' } });
  let body = '';
  for await (const chunk of req) body += chunk;
  const input = JSON.parse(body);
  const provider = input.provider?.only?.[0] || input.providerOptions?.gateway?.only?.[0] || 'alpha';
  const reply = { model: input.model, provider, choices: [{ message: { role: 'assistant', content: 'OK（示例响应）' } }], usage: { prompt_tokens: 28, completion_tokens: 8, total_tokens: 36, cost: 0.00012 } };
  if (!input.stream) return send(200, reply);
  res.writeHead(200, { 'Content-Type': 'text/event-stream' });
  res.end(`data: ${JSON.stringify({ choices: [{ delta: { content: 'OK（示例响应）' } }], provider, model: input.model })}\n\ndata: ${JSON.stringify({ usage: reply.usage })}\n\ndata: [DONE]\n\n`);
});
upstream.listen(0, '127.0.0.1');
await new Promise((resolve) => upstream.once('listening', resolve));

const portProbe = http.createServer();
portProbe.listen(0, '127.0.0.1');
await new Promise((resolve) => portProbe.once('listening', resolve));
const port = portProbe.address().port;
await new Promise((resolve) => portProbe.close(resolve));

fs.writeFileSync(path.join(dir, 'config.json'), JSON.stringify({
  port, upstreamBase: `http://127.0.0.1:${upstream.address().port}`,
  accounts, accountMode: 'roundrobin', accountConcurrency: 2,
  globalConcurrency: 3, globalRpm: 0, knownModels: models,
}, null, 2), { mode: 0o600 });
fs.writeFileSync(path.join(dir, 'metadata.json'), JSON.stringify({
  models: Object.fromEntries(models.map((model, index) => [model, {
    pipeline: 'direct', pinnable: true, canonicalSlug: model.replace('cline-pass/', 'demo/'),
    upstreams: ['alpha', 'beta', 'gamma'], lastProvider: ['alpha', 'beta', 'alpha'][index],
    lastMs: [2100, 1650, 2900][index], upstreamStatus: { alpha: { status: 'ok' }, beta: { status: 'ok' }, gamma: { status: 'limited' } },
  }])),
  stats: {}, history: [], catalog: models, catalogFetchedAt: Date.now(),
}, null, 2), { mode: 0o600 });

const now = Date.now();
const statuses = [200, 200, 200, 200, 200, 200, 200, 200, 429, 502];
const logs = Array.from({ length: 128 }, (_, index) => {
  const status = statuses[index % statuses.length];
  const account = accounts[index % accounts.length];
  const inputTokens = status === 200 ? 420 + index * 3 : 0;
  const outputTokens = status === 200 ? 110 + index : 0;
  return {
    ts: now - index * 7 * 60 * 1000,
    model: models[index % models.length], status,
    account: account.name, accountId: accountId(account.key),
    provider: ['alpha', 'beta', 'gamma'][index % 3],
    canonical: models[index % models.length].replace('cline-pass/', 'demo/'),
    endpoint: '/v1/chat/completions', stream: index % 4 !== 0,
    ms: 900 + (index % 8) * 340, inputTokens, outputTokens,
    totalTokens: inputTokens + outputTokens, cost: status === 200 ? 0.0008 + index * 0.00001 : null,
    attempts: status === 200 && index % 19 === 0 ? 2 : 1,
    error: status === 429 ? '示例：上游限流' : status === 502 ? '示例：上游暂不可用' : '',
    ip: '127.0.0.1', userAgent: 'Cline Pass Demo',
  };
});
fs.writeFileSync(path.join(dir, 'request-logs.jsonl'), logs.map((row) => JSON.stringify(row)).join('\n') + '\n', { mode: 0o600 });

const app = spawn(process.execPath, ['server.js'], { cwd: root, env: { ...process.env, DATA_DIR: dir, BIND_HOST: '127.0.0.1' }, stdio: 'inherit' });
const cleanup = () => {
  app.kill();
  upstream.close();
  fs.rmSync(dir, { recursive: true, force: true });
};
process.once('SIGINT', () => { cleanup(); process.exit(0); });
process.once('SIGTERM', () => { cleanup(); process.exit(0); });
app.once('exit', (code) => { cleanup(); process.exit(code || 0); });
console.log(`\n示例控制台：http://127.0.0.1:${port}/#overview`);
console.log('全部数据保存在临时目录，按 Ctrl+C 停止后自动删除。\n');
