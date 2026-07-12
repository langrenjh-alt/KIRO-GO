# Kiro-Go Ubuntu 24.04 二进制部署指南

本文适用于 Ubuntu 24.04 LTS `x86_64` 服务器，以 systemd 运行预编译的
`kiro-go-linux-amd64`，并以 Nginx 提供 HTTPS 反向代理。

部署前请从对应 release 记录版本标识和 SHA-256；校验和只适用于生成它的那次构建。

## 1. 重要说明

Kiro-Go 的 Admin 页面及其静态资源已经嵌入二进制。运行时只需部署
`kiro-go-linux-amd64`，不需要额外的资源目录。

本文采用以下目录布局：

```text
/opt/kiro-go/
├── current -> /opt/kiro-go/releases/RELEASE_ID
└── releases/
    └── RELEASE_ID/
        └── kiro-go

/etc/kiro-go/kiro-go.env
/var/lib/kiro-go/config.json
/var/lib/kiro-go/prompt_cache.json
/var/lib/kiro-go/responses/
```

`/var/lib/kiro-go` 包含账号令牌、API Key、Admin 密码、统计、Prompt Cache
和已存储的 Responses，必须作为敏感数据保护和备份。

## 2. 准备服务器

确认系统和架构：

```bash
cat /etc/os-release
uname -m
```

`uname -m` 必须输出 `x86_64`。然后安装运行和排障所需工具：

```bash
sudo apt update
sudo apt install -y ca-certificates curl file jq openssl tar
sudo update-ca-certificates
sudo timedatectl set-ntp true
```

该二进制为静态链接程序，不需要在服务器安装 Go。

## 3. 上传并校验文件

先在持有构建产物的机器上上传二进制：

```bash
scp dist/kiro-go-linux-amd64 ubuntu@SERVER_IP:/tmp/
```

在 Ubuntu 服务器上使用 release 提供的 SHA-256 校验当前构建：

```bash
export KIRO_GO_SHA256=REPLACE_WITH_RELEASE_SHA256
echo "${KIRO_GO_SHA256}  /tmp/kiro-go-linux-amd64" | sha256sum -c -
file /tmp/kiro-go-linux-amd64
```

预期 `file` 输出包含：

```text
ELF 64-bit LSB executable, x86-64, statically linked, stripped
```

## 4. 安装程序

创建无登录权限的专用用户：

```bash
sudo useradd \
  --system \
  --home-dir /var/lib/kiro-go \
  --create-home \
  --shell /usr/sbin/nologin \
  kiro-go
```

如果用户已经存在，可跳过 `useradd`。创建目录并安装当前 release：

```bash
export RELEASE_ID=REPLACE_WITH_RELEASE_ID
export RELEASE_DIR="/opt/kiro-go/releases/${RELEASE_ID}"

sudo install -d -o root -g root -m 0755 /opt/kiro-go/releases
sudo install -d -o root -g root -m 0755 "${RELEASE_DIR}"
sudo install -d -o root -g root -m 0750 /etc/kiro-go
sudo install -d -o kiro-go -g kiro-go -m 0700 /var/lib/kiro-go

sudo install -o root -g root -m 0755 \
  /tmp/kiro-go-linux-amd64 \
  "${RELEASE_DIR}/kiro-go"

sudo ln -sfn "${RELEASE_DIR}" /opt/kiro-go/current.next
sudo mv -Tf /opt/kiro-go/current.next /opt/kiro-go/current
```

最后确认运行布局：

```bash
test -x /opt/kiro-go/current/kiro-go
```

## 5. 配置环境变量

生成随机 Admin 密码并写入 systemd 环境文件：

```bash
ADMIN_PASSWORD="$(openssl rand -hex 24)"
sudo tee /etc/kiro-go/kiro-go.env >/dev/null <<EOF
CONFIG_PATH=/var/lib/kiro-go/config.json
ADMIN_PASSWORD=${ADMIN_PASSWORD}
LOG_LEVEL=info
EOF
sudo chown root:root /etc/kiro-go/kiro-go.env
sudo chmod 0600 /etc/kiro-go/kiro-go.env
printf 'Admin password: %s\n' "${ADMIN_PASSWORD}"
unset ADMIN_PASSWORD
```

立即将输出的密码放入密码管理器。`ADMIN_PASSWORD` 会在每次启动时覆盖配置中的
Admin 密码；只要保留该环境变量，就应通过环境文件更换密码并重启服务，不要同时
在 Admin 页面维护另一个密码。

支持的主要环境变量：

| 变量 | 说明 |
|---|---|
| `CONFIG_PATH` | 配置文件路径，建议使用绝对路径 |
| `ADMIN_PASSWORD` | 启动时覆盖 Admin 密码 |
| `LOG_LEVEL` | `debug`、`info`、`warn` 或 `error` |
| `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` | 未在 Admin 配置出站代理时使用 |
| `KIRO_PROFILE_REGIONS` | 企业账号 profile 候选区域，逗号分隔 |
| `KIRO_SSO_CALLBACK_BIND` | Hosted SSO 临时回调监听地址；通常应保持未设置 |

生产环境不要长期使用 `LOG_LEVEL=debug`，因为调试日志可能包含完整上游请求内容。

## 6. 创建安全的初始配置

程序首次自动创建的配置会监听 `0.0.0.0:8080`，且默认不要求 API Key。生产部署
建议在启动前创建最小配置，让服务只监听环回地址，并在创建 API Key 前保持 API
调用 fail-closed：

```bash
sudo tee /var/lib/kiro-go/config.json >/dev/null <<'JSON'
{
  "password": "overridden-by-ADMIN_PASSWORD",
  "port": 8080,
  "host": "127.0.0.1",
  "requireApiKey": true,
  "apiKeys": [],
  "accounts": []
}
JSON
sudo chown kiro-go:kiro-go /var/lib/kiro-go/config.json
sudo chmod 0600 /var/lib/kiro-go/config.json
sudo jq empty /var/lib/kiro-go/config.json
```

Kiro-Go 没有 `HOST` 或 `PORT` 环境变量。`host` 和 `port` 只在启动时读取，修改后
必须重启。服务运行期间不要直接编辑 `config.json`；先停止服务，避免运行时保存覆盖
手工修改。

## 7. 创建 systemd 服务

```bash
sudo tee /etc/systemd/system/kiro-go.service >/dev/null <<'UNIT'
[Unit]
Description=Kiro-Go API Proxy
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=kiro-go
Group=kiro-go
WorkingDirectory=/opt/kiro-go/current
EnvironmentFile=/etc/kiro-go/kiro-go.env
ExecStartPre=/usr/bin/test -x /opt/kiro-go/current/kiro-go
ExecStart=/opt/kiro-go/current/kiro-go
Restart=on-failure
RestartSec=5s
KillSignal=SIGTERM
TimeoutStopSec=30s
UMask=0077
LimitNOFILE=65536

NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
ReadWritePaths=/var/lib/kiro-go

[Install]
WantedBy=multi-user.target
UNIT

sudo systemctl daemon-reload
sudo systemctl enable --now kiro-go
```

程序收到 `SIGTERM` 后会给正在处理的 HTTP 请求最多 15 秒完成，然后保存统计和
Prompt Cache。不要使用 `kill -9`；`TimeoutStopSec` 也不要设置得低于 20 秒。

## 8. 验证服务

```bash
sudo systemctl status kiro-go --no-pager
curl -fsS http://127.0.0.1:8080/health | jq
curl -fsS -o /dev/null http://127.0.0.1:8080/admin
sudo journalctl -u kiro-go -n 100 --no-pager
```

健康检查应返回类似：

```json
{
  "status": "ok",
  "uptime": 10,
  "version": "1.1.2"
}
```

第二条 `curl` 验证二进制内嵌的 Admin 页面也能正常提供。`/health` 只说明 HTTP
进程正常，不代表已有可用账号或上游可访问。账号池状态可在 Admin 页面查看。

## 9. 完成首次 Admin 配置

因为服务仅监听服务器环回地址，先在管理电脑建立 SSH 隧道：

```bash
ssh -N \
  -L 8080:127.0.0.1:8080 \
  ubuntu@SERVER_IP
```

然后在管理电脑访问：

```text
http://127.0.0.1:8080/admin
```

使用第 5 步生成的 Admin 密码登录，并依次完成：

1. 添加并测试 Kiro 账号。
2. 在 API Key 管理中创建一个已启用的 Key。
3. 保持 API Key 鉴权开启。
4. 按需设置 Prompt Cache 最大比例和容量；这两项保存后立即生效，无需重启。
5. 在“流量”页面确认请求数和 Token 趋势正常。

调用 API 时使用以下任一种请求头：

```http
Authorization: Bearer YOUR_API_KEY
```

或：

```http
X-Api-Key: YOUR_API_KEY
```

## 10. 配置 Nginx 和 HTTPS

公网部署必须使用 HTTPS。下面的配置继续让 Kiro-Go 监听 `127.0.0.1:8080`，只由
Nginx 对外提供服务：

```bash
sudo apt install -y nginx certbot python3-certbot-nginx
sudo tee /etc/nginx/sites-available/kiro-go >/dev/null <<'NGINX'
server {
    listen 80;
    listen [::]:80;
    server_name api.example.com;

    client_max_body_size 32m;

    # 推荐通过 SSH 隧道管理后台，不向公网暴露 /admin。
    location ^~ /admin {
        return 404;
    }

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Connection "";

        # SSE 流式响应需要关闭缓冲并延长读取超时。
        proxy_buffering off;
        proxy_request_buffering off;
        proxy_cache off;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
        add_header X-Accel-Buffering no always;
    }
}
NGINX

sudo ln -sfn /etc/nginx/sites-available/kiro-go /etc/nginx/sites-enabled/kiro-go
sudo nginx -t
sudo systemctl reload nginx
sudo certbot --nginx --redirect -d api.example.com
curl -I http://api.example.com/health
```

将 `api.example.com` 替换为已解析到服务器的域名。如果需要通过域名访问 Admin，
应删除 `location ^~ /admin` 的 404 规则，并改为可信 IP 白名单；不要在公网 HTTP
明文环境使用 Admin。最后一条 `curl` 应返回到 `https://` 的 301 或 308 跳转。

如启用 UFW，先读取 sshd 的实际监听端口，再开放 SSH 和 Nginx：

```bash
SSH_PORT="$(sudo sshd -T | awk '/^port / {print $2; exit}')"
printf 'Detected SSH port: %s\n' "${SSH_PORT}"
sudo ufw allow "${SSH_PORT}/tcp"
sudo ufw allow 'Nginx Full'
sudo ufw enable
sudo ufw status
```

保持当前 SSH 会话不关闭，并另开一个终端验证新 SSH 连接成功后再退出。不要开放
`8080/tcp` 或 `3128/tcp`。

## 11. Hosted SSO 回调

Kiro Hosted SSO 登录期间会临时监听 `3128/tcp`，默认只绑定服务器的
`127.0.0.1` 和 `::1`。远程管理时可同时建立回调隧道：

```bash
ssh -N \
  -L 8080:127.0.0.1:8080 \
  -L 3128:127.0.0.1:3128 \
  ubuntu@SERVER_IP
```

也可以使用 Admin 提供的手动粘贴 callback URL 流程。不要为了方便把
`KIRO_SSO_CALLBACK_BIND` 设置成 `0.0.0.0` 并将 3128 直接暴露到公网。

## 12. 日常运维

常用命令：

```bash
sudo systemctl status kiro-go --no-pager
sudo systemctl restart kiro-go
sudo journalctl -u kiro-go -f
sudo journalctl -u kiro-go --since '30 minutes ago'
curl -fsS http://127.0.0.1:8080/health | jq
```

配置生效规则：

| 配置 | 是否需要重启 |
|---|---|
| Admin 中的 Prompt Cache 最大比例/容量 | 否，立即生效 |
| Admin 中的出站代理 | 否，立即生效 |
| `host`、`port` | 是 |
| systemd 环境变量 | 是 |
| 二进制 | 是 |

“流量”页面的分钟趋势保存在进程内，服务重启后会清空；累计总统计会保存在
`config.json`。

## 13. 备份

为了得到一致备份，短暂停止服务后同时备份状态目录和环境文件：

```bash
export BACKUP_DIR=/var/backups/kiro-go
export BACKUP_FILE="${BACKUP_DIR}/kiro-go-$(date +%Y%m%d-%H%M%S).tar.gz"

sudo install -d -o root -g root -m 0700 "${BACKUP_DIR}"
sudo systemctl stop kiro-go
sudo tar -C / -czf "${BACKUP_FILE}" var/lib/kiro-go etc/kiro-go
sudo systemctl start kiro-go
sudo ls -lh "${BACKUP_FILE}"
```

`prompt_cache.json` 丢失时只会导致缓存冷启动，但 `config.json` 和 `responses/`
不应随意删除。

从 `97df504` 起，Prompt Cache 指纹格式升级为 v2，用于隔离 5 分钟和 1 小时 TTL
策略。升级时旧 v1 条目会被忽略，因此首次启动会冷缓存一次；合格请求成功完成后会重新
写入，随后相同前缀即可命中。这不影响 `config.json`、账号或 Stored Responses。

## 14. 升级与回滚

升级前先将新版二进制上传到 `/tmp/kiro-go-linux-amd64`，再把下面的 release 标识和
SHA-256 替换为新版本的真实值：

```bash
export NEW_RELEASE_ID=REPLACE_WITH_NEW_RELEASE_ID
export NEW_BINARY_SHA256=REPLACE_WITH_NEW_BINARY_SHA256
export NEW_RELEASE_DIR="/opt/kiro-go/releases/${NEW_RELEASE_ID}"
export OLD_RELEASE_DIR="$(readlink -f /opt/kiro-go/current)"

echo "${NEW_BINARY_SHA256}  /tmp/kiro-go-linux-amd64" | sha256sum -c -
```

确认新的 release 目录尚不存在，再安装二进制：

```bash
if sudo test -e "${NEW_RELEASE_DIR}"; then
  echo "Release directory already exists: ${NEW_RELEASE_DIR}" >&2
  exit 1
fi

sudo install -d -o root -g root -m 0755 "${NEW_RELEASE_DIR}"
sudo install -o root -g root -m 0755 \
  /tmp/kiro-go-linux-amd64 \
  "${NEW_RELEASE_DIR}/kiro-go"
```

切换版本前必须创建一致性备份。备份失败时重新启动旧版本并终止升级：

```bash
export BACKUP_DIR=/var/backups/kiro-go
export UPGRADE_BACKUP="${BACKUP_DIR}/pre-${NEW_RELEASE_ID}-$(date +%Y%m%d-%H%M%S).tar.gz"

sudo install -d -o root -g root -m 0700 "${BACKUP_DIR}"
sudo systemctl stop kiro-go
if ! sudo tar -C / -czf "${UPGRADE_BACKUP}" var/lib/kiro-go etc/kiro-go; then
  sudo systemctl start kiro-go
  echo 'Backup failed; upgrade aborted.' >&2
  exit 1
fi
sudo test -s "${UPGRADE_BACKUP}"

sudo ln -sfn "${NEW_RELEASE_DIR}" /opt/kiro-go/current.next
sudo mv -Tf /opt/kiro-go/current.next /opt/kiro-go/current
sudo systemctl start kiro-go
curl -fsS http://127.0.0.1:8080/health | jq
```

如果健康检查失败，立即回滚链接：

```bash
sudo systemctl stop kiro-go
sudo ln -sfn "${OLD_RELEASE_DIR}" /opt/kiro-go/current.next
sudo mv -Tf /opt/kiro-go/current.next /opt/kiro-go/current
sudo systemctl start kiro-go
```

新版本启动时可能迁移并保存 `config.json`。回滚到旧二进制前应评估配置兼容性；
必要时恢复升级前的 `/var/lib/kiro-go` 备份。升级不是零停机，超过 15 秒的在途 SSE
请求可能会被断开。

## 15. 常见问题

### Admin 返回 404 或页面资源缺失

Admin 资源位于二进制内部。先确认服务和页面响应，再检查当前运行的二进制校验和与
release 是否一致：

```bash
curl -fsS -o /dev/null http://127.0.0.1:8080/admin
sha256sum /opt/kiro-go/current/kiro-go
sudo journalctl -u kiro-go -n 100 --no-pager
```

如果健康检查正常但 Admin 资源缺失，请重新部署包含内嵌资源的正确 release 二进制。

### 无法写入配置或 Prompt Cache

```bash
sudo chown -R kiro-go:kiro-go /var/lib/kiro-go
sudo chmod 0700 /var/lib/kiro-go
sudo chmod 0600 /var/lib/kiro-go/config.json
```

### 8080 或 3128 端口被占用

```bash
sudo ss -ltnp | grep -E ':(8080|3128)\b'
```

### 服务反复重启

```bash
sudo systemctl status kiro-go --no-pager
sudo journalctl -u kiro-go -n 200 --no-pager
sudo jq empty /var/lib/kiro-go/config.json
```

### 反向代理下流式输出延迟

确认 Nginx 已设置 `proxy_buffering off`，且 CDN 或上游代理没有再次缓冲 SSE 响应。

### 健康检查正常但 API 不可用

`/health` 不检查账号池。登录 Admin，确认至少一个账号已启用且测试成功，并检查
服务日志中的认证、配额、区域或出站网络错误。
