# Cline Pass 上游控制台（cline-pass-switcher）

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
![Node](https://img.shields.io/badge/node-%E2%89%A5%2018-green)
![Docker](https://img.shields.io/badge/docker-ready-2496ED)

**零依赖**的 Node.js 本地/服务器代理 + 网页控制台，用于 [Cline Pass](https://cline.bot/cline-pass) 订阅：

- 🔍 **上游枚举与校验** —— 列出订阅模型背后每一条上游渠道，并一键实测哪些「✔可用 / ⏳限流 / ✘不可钉」
- 🎯 **精确钉住上游** —— 严格钉住 / 优先+回退两种模式，支持按最低成本、最快首字、最高吞吐排序
- 🧬 **多上游优先级故障转移** —— 勾选多个上游即按顺序逐个尝试：当前渠道按调度策略重试后仍失败，再顺切下一个；全部失败才透传错误。每次尝试有独立 120s 超时与逐次尝试明细（请求头 X-Cline-Target-Upstream: a>b 与 X-Cline-Attempts，历史与测试台展示尝试路径）
- 🚫 **上游排除** —— 勾「排除」的渠道永不被使用：勾选模式下从候选中剔除；自动模式与优先+回退模式下把排除换算成 only 白名单（已知上游 - 排除项）注入，两类管道均实测生效；网关侧渠道清单更新导致白名单过期时，报错中附带的最新渠道清单会被自动学习合并
- 👥 **账号池** —— 多账号管理、手动切换、轮询均衡、逐账号连通性测试与用量统计
- 🚦 **单账号并发限制** —— 为每个账号设置同时执行的请求数，超出的请求进入该账号队列，不影响其他账号并行
- 🚥 **流量与调度** —— 全局并发、滑动 60 秒 RPM、重试次数和间隔、429 换号、传输错误换号或同号重试；流式回复开始后不重试
- 📥 **批量账号操作** —— 一次粘贴多行 key 导入，并可勾选多个账号批量测试
- 💳 **额度查看** —— 参考 codex2api 的进度条布局，展示 Cline 接口实际返回的额度窗口；余额接口有总量时展示总额度比例，无权读取时显示原因。`spark` 是 codex2api 的 Codex 专属额度，不会虚构为 Cline Pass 额度
- 📈 **使用统计与请求日志** —— 今日请求、Token、上游自报费用、RPM/TPM、错误率、模型分布和请求构成；日志按时间、状态、模型、账号、端点、流式方式与关键词筛选，服务端分页，自动只保留近 2 天
- 📊 **观测** —— 每条请求自动记录实际命中的渠道、背后模型、耗时（含流式）
- 🔑 **双密钥隔离** —— 控制台登录使用 `adminKey`，下游客户端使用 `proxyKey`；客户端 API Key 不能管理账号与安全设置
- 🌐 **OpenAI 兼容** —— 任何 OpenAI 客户端 / Cline 扩展把 Base URL 指向代理即可，无侵入

控制台以运行中的 `public/index.html` 为准；旧版截图保留在 `docs/` 仅供版本对比。


---

## 30 秒上手（本地）

```bash
git clone https://github.com/<你的用户名>/cline-pass-switcher.git
cd cline-pass-switcher
node server.js        # 仅需 Node ≥ 18，无需 npm install
```

启动日志会显示首次生成的控制台管理员密钥。打开 <http://127.0.0.1:3123/>，用该密钥登录，然后在「账号管理」里添加你的 Cline Pass 账号（`sk_` 开头的 key）并保存。
没有 key 也能启动：页面会提示配置入口。

想先查看带数据的控制台，可运行 `npm run demo`。命令会在本机启动使用虚构账号、额度和日志的演示服务，并打印访问地址；按 `Ctrl+C` 停止后会删除临时演示数据。

> Cline Pass key 从哪里来？购买 Cline Pass 订阅后，在 Cline 的账户设置里创建 API Key。
> 订阅模型 ID 均为 `cline-pass/*` 前缀（如 `cline-pass/glm-5.2`）。

客户端接入（任何 OpenAI 兼容工具）：

```
Base URL: http://127.0.0.1:3123/v1
API Key:  （使用控制台「访问与安全」中的客户端 API Key，即 proxyKey）
Model:    cline-pass/glm-5.2 等
```

---

## Docker 部署

### 方式 A：All-in-one（自带 Caddy 自动 HTTPS，推荐新手）

```bash
mkdir -p data && cp config.example.json data/config.json
# 编辑 data/config.json，或在启动时用环境变量注入 key

# 有域名（A 记录指向服务器，自动签发 Let's Encrypt 受信证书）：
CPASS_DOMAIN=pass.example.com docker compose -f deploy/docker-compose.all-in-one.yml up -d --build

# 只有 IP（自签证书，浏览器需手动信任一次）：
docker compose -f deploy/docker-compose.all-in-one.yml up -d --build
```

访问 `https://你的域名/`（或 `https://服务器IP/`），用启动日志中的控制台密钥登录。客户端使用页面上的独立 API Key。

### 方式 B：已有一个性化反代（nginx 门户等）

根目录的 `docker-compose.yml` 只启动应用并绑定 `127.0.0.1:3123`，由你现有的 nginx/Caddy 做 TLS：

```nginx
location / {
    proxy_pass http://127.0.0.1:3123;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto https;
    proxy_buffering off;            # 流式响应必须
    proxy_read_timeout 600s;
}
```

### 环境变量

| 变量 | 说明 |
|---|---|
| `CLINE_PASS_KEY` | 上游 Cline Pass API Key（无 config 时自动创建账号） |
| `PROXY_KEY` | 下游代理密钥（客户端访问代理的凭据） |
| `ADMIN_KEY` | 控制台管理员密钥（仅用于 `/api/*` 管理接口） |
| `PUBLIC_BASE_URL` | 门户展示的公网代理地址，如 `https://pass.example.com` |
| `PORT` / `BIND_HOST` / `DATA_DIR` | 端口 / 绑定地址（容器内为 0.0.0.0）/ 配置目录 |

环境变量在启动时覆盖 `config.json`；此后通过控制台保存设置，会以当前生效值写回文件。未配置的 `adminKey`、`proxyKey` 会自动生成并写入 `config.json`，仅新生成的管理员密钥会在启动日志显示一次。两把密钥不能相同，管理接口必须用 `X-Admin-Key: <adminKey>`，客户端接口必须用 `Authorization: Bearer <proxyKey>`。

从旧版升级时先停止服务并备份 `config.json`、`metadata.json` 和 `request-logs.jsonl`。原 `proxyKey` 会保持不变，新 `adminKey` 会自动生成；首次启动请保存日志里显示的管理员密钥。若原来未设置 `proxyKey`，升级后客户端需使用自动生成的 API Key，可登录控制台查看。旧浏览器保存的代理密钥不再具有管理权限，需要重新登录。

---

## 配置参考（config.json）

| 字段 | 说明 |
|---|---|
| `accounts` | 账号池：`[{ name, key, enabled }]` |
| `accountMode` | `single` 手动指定 / `roundrobin` 轮询 |
| `activeAccount` | 单账号模式下使用的下标 |
| `accountConcurrency` | 每个账号允许的同时请求数（1-32，默认 1；超出后排队） |
| `globalConcurrency` | 全账号合计最大并发（1-128，默认 3） |
| `globalRpm` | 滑动 60 秒内最多启动的上游请求数（0=不限，默认 0） |
| `maxRetries` | 单个代理请求最多额外重试次数（0-10，默认 2）；渠道优先级故障转移不消耗此计数 |
| `rateLimitSwitchRetries` | 遇到 429 时最多换账号重试次数（0-10，默认 1），同时受 `maxRetries` 限制；仅轮询模式且有其他启用账号时生效 |
| `retryIntervalMs` | 每次重试前等待毫秒数（0-30000，默认 0） |
| `transportRetryStrategy` | 传输错误后 `switch` 换号或 `sticky` 同号（默认 `switch`）；单账号模式保持指定账号 |
| `proxyKey` | 客户端 API Key；缺失时自动生成，不能留空 |
| `adminKey` | 控制台管理员密钥；与 `proxyKey` 不同，缺失时自动生成 |
| `streamIdleTimeoutMs` | 流式上游连续无数据的超时毫秒数（1000-600000，默认 120000） |
| `publicBaseUrl` | 公网代理地址（控制台展示用） |
| `exposeCatalog` | `true` 时代理的 `/v1/models` 会合并 Cline 公开目录模型；默认 `false` 只返回订阅模型（避免客户端模型列表被淹没） |
| `knownModels` | 订阅模型清单（控制台主表） |
| `perModel` | 每模型的钉住配置：`{ upstreams, exclude, pinMode: strict|preferred, sort: cost|ttft|tps }` |
| `apiKey` | 旧版单账号字段，启动时迁移进 `accounts` 后移除，避免禁用账号时继续作为后备凭据 |

---

## 核心机制：Cline Pass 的两条路由管道（实测发现）

Cline Pass 订阅模型在 Cline 网关之后分成两条管道，钉住上游的写法**完全不同**：

| 管道 | 实际后端 | 识别特征 | 钉住方式 |
|---|---|---|---|
| **直连**（direct） | OpenRouter | 响应顶层带 `provider` 与真实 `model` 字段 | 顶层 `provider.only / order` |
| **规划器**（planner） | **Vercel AI Gateway** | 响应带 `provider_metadata.gateway.routing` | **`providerOptions.gateway.only / order / sort`** |

**关键发现**：规划器管道的请求由 Vercel AI Gateway 执行，请求体里的顶层 `provider.only/order` 会被 Cline 丢弃
（这也是官方 API 上"换上游不生效"的原因），但 `providerOptions.gateway` 嵌套形式会**原样透传**：

```json
{
  "model": "cline-pass/glm-5.2",
  "messages": [],
  "providerOptions": { "gateway": { "only": ["alibaba"] } }
}
```

实测响应：`finalProvider: "alibaba"`，规划器理由变为 `Provider set restricted to: alibaba`。
参考：[Vercel AI Gateway — Provider Filtering, Ordering & Sorting](https://vercel.com/docs/ai-gateway/models-and-providers/provider-filtering-and-ordering)

### 上游枚举的三种手段

1. **响应元数据回读**：规划器管道带 `canonicalSlug` / `fallbacksAvailable` / `finalProvider`；直连管道顶层 `provider` 即实际上游；
2. **假上游探测**（零 token）：带不存在的 `only:["__probe__"]` 让网关在路由层报错并列出精确的可用渠道清单（两条管道的清单**不一致**，要分别取）；
3. **OpenRouter 公开接口** `GET /api/v1/models/{slug}/endpoints`：补充上下文长度/在线率（对直连管道有直接参考意义）。

### 实测记录（2026-09）

| 实验 | 结果 |
|---|---|
| glm-5.2 + 顶层 `provider.only/ignore/order` | 全部被网关丢弃，恒选同一渠道 |
| glm-5.2 + `providerOptions.gateway.only:["alibaba"]` | ✔ `finalProvider: alibaba` |
| glm-5.2 流式 + `only:["baseten"]` | ✔ 流式同样生效 |
| glm-5.2 + `providerOptions.gateway.sort:"cost"` | ✔ 按成本重排执行顺序 |
| glm-5.3-flash（直连）+ 顶层 `provider.only:["gmicloud"]` | ✔ `provider: "GMICloud"` |
| glm-5.3-flash + `providerOptions.gateway` | ✘ 无效（直连管道只认顶层 provider 形式） |

> 管道归属由 Cline 侧决定、可能随时间变化，控制台的「探测」会刷新每个模型的管道类型与渠道清单。

---

## 控制台功能一览

控制台使用顶部导航切换页面；地址栏的 `#overview`、`#accounts`、`#models`、`#testing`、`#stats`、`#logs`、`#security` 可直接打开对应页面。页面显示运行中的真实数据，未配置账号或上游用量接口不可用时会明确显示状态。

| 页面 | 当前功能 |
|---|---|
| 概览 | 已启用与总账号数、当前运行与排队数、今日请求与错误数、订阅模型数；可跳转到详细页面 |
| 账号池 | 账号增删改、批量导入、勾选批量测试、单账号/轮询模式、逐账号额度与并发状态、全局流量调度 |
| 模型管理 | 背后模型、渠道数、最近实际上游、上游多选优先级与排除、严格钉住/优先回退、排序；可探测、测试和校验 |
| 测试台 | 任选模型+渠道发一条小请求，直接看网关是否采纳 |
| 使用统计 | 今日请求、Token、上游自报费用、RPM/TPM、错误率、模型按请求或已知费用排名、流式占比；费用缺失时显示不可用，不按未知价格估算 |
| 请求日志 | 记录状态、模型、账号、实际渠道、端点、Token、耗时、IP、客户端和错误；按时间、状态、模型、账号、端点、流式方式、关键词查询，每页 25/50/100 条；近 2 天自动清理 |
| 安全与目录 | 分别修改控制台密钥、客户端 API Key、公网代理地址和模型目录开关；浏览 Cline 公开目录，`:free` 变体可精确钉住 |

代理同时做了兼容性标准化：解包 Cline 的 `{"data":...}` 包装为标准 OpenAI 格式、错误统一为
`{"error":{"message":...}}`、附加 `X-Cline-Target-Upstream / X-Cline-Actual-Upstream / X-Cline-Account` 等响应头。

---

## 常见问题

**Q：为什么选了某个渠道会报 `invalid_request_error`？**
部分渠道被单独钉住时会因模型 ID 映射失败，还有渠道处于共享池限流（429）状态。点该模型行的「校验」，
把所有渠道实测一遍，下拉框会标注 ✔可用 / ⏳限流 / ✘不可钉。钉住失败的渠道会被自动学习标记。

**Q：限流的渠道还能用吗？**
能。限流是共享池的临时状态，过段时间重新「校验」即可；或改用「优先+回退」模式，限流时自动跳到其他渠道。

**Q：直接用官方 API 写 `provider.only` 为什么不生效？**
对规划器管道（走 Vercel AI Gateway 的模型）会被 Cline 网关丢弃，请改用 `providerOptions.gateway`，见上文。

**Q：两条管道的渠道清单为什么不一样？**
钉住发生在不同后端（OpenRouter vs Vercel AI Gateway），各自支持的渠道池不同，要以对应清单为准。

**Q：订阅额度怎么计？**
经代理的请求与直连官方 API 计费一致；「探测/测试/校验」会产生极小额的真实请求（每次约 0.0002 美元级）。

---

## 安全提醒

- `config.json` / `data/` 含明文密钥，日志文件 `request-logs.jsonl` 含请求来源 IP 和客户端信息，均已在 `.gitignore` 排除，**不要提交或分享**；配置、元数据和日志文件权限设为仅当前用户可读写；
- 控制台密钥与客户端 API Key 必须分别保管，均可在控制台轮换；
- `maxRetries > 0` 时可能放大请求量，注意额度消耗。账号测试、额度查询和渠道探测同样遵守全局并发与 RPM，但不使用聊天重试策略。

## License

[MIT](LICENSE)
