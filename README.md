# Cline Pass 上游控制台（cline-pass-switcher）

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
![Go](https://img.shields.io/badge/go-1.22%2B-00ADD8)
![Docker](https://img.shields.io/badge/docker-ready-2496ED)

使用 Go 标准库实现的轻量级本地 / 服务器反向代理服务与 Web 管理控制台，专为 [Cline Pass](https://cline.bot/cline-pass) 订阅打造。

对外提供标准 **OpenAI 兼容接口**（`/v1/chat/completions`、`/v1/models`），支持无缝接入 Cline 扩展、Roo Code、Cursor、Chatbox、NextChat 等任意客户端。

---

## 目录

- [项目背景与核心价值](#项目背景与核心价值)
- [核心功能特性](#核心功能特性)
- [深入技术原理：Cline Pass 双路由管道](#深入技术原理cline-pass-双路由管道)
- [多场景部署指南](#多场景部署指南)
  - [方式一：本地 / 源码运行（Go）](#方式一本地--源码运行go)
  - [方式二：Docker 容器化部署（推荐）](#方式二docker-容器化部署推荐)
  - [方式三：Linux Systemd 守护进程部署](#方式三linux-systemd-守护进程部署)
  - [生产反向代理与 HTTPS 配置（Nginx）](#生产反向代理与-https-配置nginx)
- [客户端接入配置](#客户端接入配置)
- [配置详解（config.json 与环境变量）](#配置详解configjson-与环境变量)
- [控制台功能概览](#控制台功能概览)
- [常见问题与排错（FAQ）](#常见问题与排错faq)
- [安全须知](#安全须知)
- [开源协议](#开源协议)

---

## 项目背景与核心价值

Cline Pass 是 Cline 官方推出的高性价比订阅，汇集了众多主流顶尖大模型（如 GLM-5 系列、Kimi 系列、DeepSeek 系列、Qwen 系列等）。但在实际使用过程中，开发者常遇到以下痛点：

1. **上游渠道完全黑盒且不可控**：网关背后对接了大量第三方上游渠道（如 Alibaba, Baseten, GMICloud 等），不同渠道的响应延迟（TTFT）、吞吐速度（TPS）以及稳定性差异巨大；
2. **官方 API 钉渠道失效**：直接在请求中添加 `provider.only` 等字段往往被网关丢弃，无法指定高质量物理渠道；
3. **高频使用容易 429 限流**：账号受并发与 RPM 限制，遇到突发限流时无法自动切换多账号；
4. **额度窗口不透明**：Cline Pass 采用多周期滚动窗口（如 5小时 / 本周 / 本月），官方没有直观看板；
5. **多账号管理繁琐**：无法在一个网关中聚合多个账号实现轮询与故障转移。

本项目正是为了解决上述痛点而生，让你的 Cline Pass 订阅发挥出最极致、稳定的性能。

---

## 核心功能特性

### 1. 🔍 精准钉住上游与渠道探测
- **自动适配双管道**：深入 Cline 网关底层，识别「直连管道」与「规划器管道」，自动采用正确的协议结构注入上游过滤；
- **多种钉住模式**：
  - **严格钉住（Strict）**：强制只走选定的上游渠道，杜绝盲目漂移；
  - **优先 + 回退（Preferred）**：首选指定的优质渠道，遇故障或限流时自动回退，保障可用性；
- **多渠道优先级故障转移（Failover）**：勾选多个上游（如 `Alibaba > Baseten > GMICloud`），前一个失败/超限后顺切下一个，全部失败才透传错误；
- **渠道排除黑名单**：勾选排除的渠道永不被使用，支持网关变动后的自适应学习；
- **一键实测校验**：控制台提供一键校验，自动测试全部上游并标注「✔ 可用 / ⏳ 限流 / ✘ 不可钉」。

### 2. 👥 账号池与智能并发/流量调度
- **单账号与轮询池（Round-Robin）**：支持多账号配置、状态启停、批量多行 Key 导入以及批量连通性测试；
- **按账号独立并发队列**：为每个账号配置独立最大并发（`accountConcurrency`），超出排队，互不干扰；
- **全局并发与 RPM 保护**：全局最大并发与滑动 60 秒全局 RPM 限制，防止调用过猛触发官方风控；
- **排队超时解耦**：排队等待不消耗上游重试与超时预算，排队超时明确返回 `503 overload`；
- **429 智能熔断与冷却**：
  - 遇到 429 限流时，若账号周期用满，该账号在内存中冷却至 `resetsAt` 重置时间；
  - 若仅为单个渠道限流，仅冷却该渠道，其余渠道正常使用；
  - 冷却期内直接拦截无效请求，避免浪费额度与加剧限流。

### 3. 💳 真实官方额度与用量监控
- **真实窗口透视**：直连官方 `/users/me/plan/usage-limits`，精准还原 5 小时、本周、本月的用量百分比与重置时间（`resetsAt`）；
- **官方余额显示**：读取官方 `/balance` 的真实 credits（除以 100 换算）；
- **防虚构**：严格区分真实 Cline Pass 额度与第三方专有额度（如 codex2api spark 额度），真实准确。

### 4. 📈 观测体系与审计日志
- **全链路追踪**：每条请求记录状态、耗时（首字 TTFT 与流式总耗时）、真实底层模型（`canonicalSlug`）、实际命中渠道（`finalProvider`）、Token 用量等；
- **标准响应头透出**：通过 `X-Cline-Target-Upstream`、`X-Cline-Actual-Upstream`、`X-Cline-Attempts` 追溯每次调用；
- **审计日志看板**：支持按时间、状态码、模型、账号、端点、流式及关键词检索，服务端分页，自动保留近 2 天日志。

### 5. 🔑 权限隔离与安全性
- **双密钥严格分离**：
  - `adminKey`：专用于 Web 控制台登录与 `/api/*` 管理接口；
  - `proxyKey`：仅用于下游客户端接入 `/v1/chat/completions`，客户端 Key 泄露无法篡改服务端配置；
- **密钥安全脱敏**：前端编辑时账号 Key 采用特殊圆点掩码保护，防止明文意外泄露或被空白覆盖。

---

## 深入技术原理：Cline Pass 双路由管道

Cline Pass 订阅模型在进入官方网关后，实际上会分流到两条完全不同的底层后端架构：

| 管道类型 | 实际后端 | 响应特征识别 | 钉住上游生效写法 |
|---|---|---|---|
| **直连管道（direct）** | OpenRouter | 响应顶层携带 `provider` 与真实 `model` 字段 | 顶层 `provider: { only: [...], order: [...] }` |
| **规划器管道（planner）** | **Vercel AI Gateway** | 响应携带 `provider_metadata.gateway.routing` | **`providerOptions: { gateway: { only: [...], order: [...] } }`** |

> **关键原理发现**：
> 走 Vercel AI Gateway 的模型（如 GLM-5.2 等），官方网关会直接把请求顶层的 `provider` 属性剥除丢弃（这正是很多用户直接在客户端配置无法选渠道的原因）。
> 但通过透传嵌套的 `providerOptions.gateway`，Vercel AI Gateway 能正确接收到路由约束并完成渠道钉住。

本项目在收到请求时，会自动查询并根据当前模型的管道类型，动态组装出正确的负载结构，确保任何模式下渠道控制均能生效。

---

## 多场景部署指南

本项目采用纯 Go 标准库编写，零重型外部依赖，内存占用极低（通常约 20MB ~ 50MB）。

### 方式一：本地 / 源码运行（Go）

#### 1. 环境准备
- 安装 Go 1.22 及以上版本（推荐 1.25+）。

#### 2. 下载并启动
```bash
# 克隆仓库
git clone https://github.com/gxu689/cline-pass-alone.git
cd cline-pass-alone

# 直接运行（首次启动会自动生成 adminKey 与 proxyKey）
go run .
```

#### 3. 访问与配置
- 终端会输出类似日志：
  ```text
  [安全] 已生成控制台管理员密钥（仅此一次显示）：<你的随机管理员密钥>
  Cline Pass Go 控制台: http://127.0.0.1:3123/
  OpenAI 兼容代理地址: http://127.0.0.1:3123/v1
  ```
- 浏览器打开 `http://127.0.0.1:3123/`，使用该管理员密钥登录。
- 进入「**账号管理**」，添加你的 Cline Pass API Key（以 `sk_` 开头）并保存。

---

### 方式二：Docker 容器化部署（推荐）

项目内已自带针对生产优化的多阶段构建 `Dockerfile` 与 `docker-compose.yml`。

#### 1. 使用 Docker Compose（最便捷）

```bash
# 进入项目目录
cd cline-pass-alone

# 后台构建并启动
docker compose up -d --build

# 查看运行日志（初次启动请在日志中查看生成的管理员密钥）
docker compose logs -f cline-pass-console
```

#### 2. 自定义环境变量（可选）
你可以直接在 `docker-compose.yml` 中指定环境密钥，省去控制台手动配置账号：

```yaml
services:
  cline-pass-console:
    build: .
    container_name: cline-pass-console
    restart: unless-stopped
    ports:
      - "127.0.0.1:3123:3123"
    environment:
      - CLINE_PASS_KEY=sk_your_cline_pass_key_here
      - ADMIN_KEY=YourCustomStrongAdminKey123
      - PROXY_KEY=YourCustomClientProxyKey123
      - PORT=3123
      - BIND_HOST=0.0.0.0
    volumes:
      - cline-pass-data:/app/data

volumes:
  cline-pass-data:
```

#### 3. 单容器命令运行
```bash
# 构建镜像
docker build -t cline-pass-switcher .

# 启动容器并挂载数据卷
docker run -d \
  --name cline-pass \
  -p 127.0.0.1:3123:3123 \
  -v $(pwd)/data:/app/data \
  --restart unless-stopped \
  cline-pass-switcher
```

---

### 方式三：Linux Systemd 守护进程部署

适用于在云服务器独立 Linux 系统上无 Docker 环境长期运行。

#### 1. 编译静态二进制
```bash
# 在源码目录编译
go build -ldflags="-s -w" -o cline-pass-server .
# 将可执行文件与静态资源目录复制到运行目录（例如 /opt/cline-pass）
sudo mkdir -p /opt/cline-pass
sudo cp cline-pass-server /opt/cline-pass/
sudo cp -r public /opt/cline-pass/
```

#### 2. 创建 Systemd 服务文件
```bash
sudo tee /etc/systemd/system/cline-pass.service > /dev/null << 'EOF'
[Unit]
Description=Cline Pass Switcher Proxy Service
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/cline-pass
ExecStart=/opt/cline-pass/cline-pass-server
Restart=always
RestartSec=5
LimitNOFILE=65536
Environment="PORT=3123"
Environment="BIND_HOST=127.0.0.1"

[Install]
WantedBy=multi-user.target
EOF
```

#### 3. 启动并启用开机自启
```bash
sudo systemctl daemon-reload
sudo systemctl enable --now cline-pass
sudo systemctl status cline-pass
```

---

### 生产反向代理与 HTTPS 配置（Nginx）

由于本项目内置 HTTP 纯服务，如需公网访问或域名绑定，强烈建议使用 Nginx 反代并配置 SSL 证书。
特别注意：**流式输出（SSE）需要禁用 proxy_buffering**。

```nginx
server {
    listen 80;
    server_name cline.yourdomain.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    server_name cline.yourdomain.com;

    ssl_certificate /path/to/fullchain.pem;
    ssl_certificate_key /path/to/privkey.pem;

    # 客户端及流式聊天请求支持
    location / {
        proxy_pass http://127.0.0.1:3123;
        proxy_http_version 1.1;

        # 核心：必须禁用缓冲区以支持 SSE 流式推流
        proxy_buffering off;
        proxy_cache off;

        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # 长连接超时设置（避免大模型输出时连接中断）
        proxy_read_timeout 600s;
        proxy_send_timeout 600s;
    }
}
```

---

## 客户端接入配置

任何支持 OpenAI 格式的客户端均可无缝接入。

### 1. Cline / Roo Code 官方插件接入
在 VSCode 扩展设置中，选择服务商为 **OpenAI Compatible**：
- **Base URL**：`http://127.0.0.1:3123/v1`（或你的反代域名 `https://cline.yourdomain.com/v1`）
- **API Key**：填入控制台「访问与安全」中生成的 **`proxyKey`**
- **Model ID**：填入订阅的模型（例如 `cline-pass/glm-5.2`、`cline-pass/kimi-k3` 等）

### 2. Cursor / Continue / Chatbox 等客户端
- **API Base URL**：`http://127.0.0.1:3123/v1`
- **API Key**：`<proxyKey>`
- **Model**：`cline-pass/glm-5.2` 等

---

## 配置详解（config.json 与环境变量）

### 1. 环境变量参考

| 环境变量 | 说明 | 示例 |
|---|---|---|
| `CLINE_PASS_KEY` | 上游 Cline Pass API Key（初次启动无 config 时自动录入） | `sk_live_...` |
| `ADMIN_KEY` | 控制台管理员密钥（仅用于 Web 管理界面） | `my-secret-admin-key` |
| `PROXY_KEY` | 客户端访问代理的 API Key（供客户端调用 `/v1`） | `my-client-proxy-key` |
| `PORT` | 服务监听端口（默认 3123） | `3123` |
| `BIND_HOST` | 服务绑定地址（本地默认 127.0.0.1，容器内 0.0.0.0） | `0.0.0.0` |
| `DATA_DIR` | 数据文件存储目录（默认当前目录） | `/app/data` |
| `PUBLIC_BASE_URL` | 控制台展示用的公网代理地址 | `https://cline.example.com` |
| `CORS_ORIGIN` | 允许的跨域来源 | `*` |

### 2. config.json 关键配置字段

```json
{
  "port": 3123,
  "adminKey": "...",
  "proxyKey": "...",
  "accountMode": "single",             // 账号模式：single (单账号) / roundrobin (轮询)
  "activeAccount": 0,                  // 单账号模式时选用的账号下标
  "accountConcurrency": 1,             // 单账号同时执行的最大并发（1-32，超出自动排队）
  "globalConcurrency": 3,              // 全账号合计最大并发（1-128）
  "globalRpm": 0,                      // 滑动 60 秒内最大上游请求数（0 为不限制）
  "maxRetries": 2,                     // 代理请求失败最多重试次数（0-10）
  "rateLimitSwitchRetries": 1,         // 遇到 429 时最多换号重试次数（仅轮询模式生效）
  "retryIntervalMs": 0,                // 重试前等待毫秒数
  "streamIdleTimeoutMs": 120000,       // 流式响应无数据超时时间（毫秒）
  "transportRetryStrategy": "switch",  // 网络错误时的策略：switch (换号) / sticky (同号)
  "exposeCatalog": false,              // /v1/models 是否合并返回 Cline 全部公开目录
  "accounts": [                        // 账号池列表
    {
      "name": "account-1",
      "key": "sk_...",
      "enabled": true
    }
  ],
  "knownModels": [ ... ],              // 订阅模型列表
  "perModel": { ... }                  // 每个模型的专属钉住配置与排除策略
}
```

---

## 控制台功能概览

Web 控制台内置在服务中，无需额外构建前端：

| 页面 | 主要功能 |
|---|---|
| **概览 (#overview)** | 展示账号启用情况、当前全局运行/排队并发数、今日请求/错误统计、订阅模型数量 |
| **账号池 (#accounts)** | 账号增删改查、多行批量导入、逐账号/批量测试、单账号/轮询切换、逐账号实时额度与并发监控 |
| **模型管理 (#models)** | 查看模型背后的实际上游渠道、配置多上游优先级、设置严格钉住/优先回退、渠道排除与健康校验 |
| **测试台 (#testing)** | 调试控制台，直接向指定模型+指定渠道发送请求，即时查看网关采纳结果与响应耗时 |
| **使用统计 (#stats)** | 今日请求趋势、Token 统计、自报费用估算、实时 RPM/TPM 曲线、模型调用构成比 |
| **请求日志 (#logs)** | 详尽记录每次请求的客户端 IP、耗时、渠道追踪、状态码与错误信息，支持多维过滤 |
| **访问与安全 (#security)** | 轮换管理端与客户端密钥、设置公网 Base URL、管理模型目录展示偏好 |

---

## 常见问题与排错（FAQ）

### Q1：为什么选定某个渠道后请求报错 `invalid_request_error`？
部分上游渠道在网关层做模型映射时可能存在兼容性问题，或者该渠道当前处于共享池超限状态。
**解决办法**：在控制台「模型管理」页面点击该模型的「**校验**」，系统会实测所有渠道并标注状态；或者将钉住模式改为「**优先 + 回退**」，失败时自动切换。

### Q2：为什么我在直接调用官方 API 时加 `provider.only` 不生效？
因为走 Vercel AI Gateway 规划器管道的模型，顶层 `provider` 字段会被官方网关丢弃。本项目会自动转换为嵌套的 `providerOptions.gateway.only` 结构透传，确保生效。

### Q3：为什么日志提示「上游并发已满，排队超时」？
这是因为当前账号的并发请求数达到了 `accountConcurrency` 限制，后续请求进入队列等待；如果队列等待超过了 `streamIdleTimeoutMs`（默认 120 秒），则会主动返回 503 保护上游。
**解决办法**：如需更高并发，可在「账号管理」中适当调大并发设置，或添加更多账号开启轮询模式。

### Q4：官方额度显示是怎么计算的？
Cline 官方的 Credits 余额按 `/balance` 中的数值除以 100 进行换算。本控制台还直接解析了官方 `/users/me/plan/usage-limits` 接口，真实展示 5小时 / 本周 / 本月的窗口使用比例与重置倒计时。

---

## 安全须知

1. **凭证脱敏与文件权限**：`config.json`、`data/` 与 `request-logs.jsonl` 均已包含在 `.gitignore` 中，**切勿提交到公共仓库**；
2. **密钥分别保管**：`adminKey` 拥有控制台完整管理权，`proxyKey` 仅供客户端代理使用，请务必分开配置；
3. **公网防护**：请勿将管理端口直接暴露在公网，如需公网访问建议配合 Nginx、Basic Auth、VPN 或安全组限制来源 IP。

---

## License

本项目基于 [MIT License](LICENSE) 开源。
