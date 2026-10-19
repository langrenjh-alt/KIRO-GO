# 独立 Python / Bash 执行 Worker

该服务是主程序之外的独立 Go 进程，提供真实 Python / Bash 执行接口。可信层负责调用 Docker CLI；提交的代码只交给隔离容器内的固定解释器，不在宿主机执行。服务不自动安装解释器，不自动拉取镜像，也不在运行失败时生成替代执行结果。

## 构建与配置

```bash
go build -o kiro-code-executor ./cmd/kiro-code-executor
go test ./codeexec ./cmd/kiro-code-executor
go vet ./codeexec ./cmd/kiro-code-executor
```

| 环境变量 | 含义 |
| --- | --- |
| `KIRO_CODE_EXECUTOR_TOKEN_FILE` | 必填，包含共享 Bearer token 的本地文件。不存在、为空、超过 4 KiB 或包含内部空白时拒绝启动。允许文件结尾换行。 |
| `KIRO_CODE_EXECUTOR_LISTEN` | 默认 `127.0.0.1:12346`。只接受字面回环 IP；通配地址、外部地址和主机名会被拒绝。 |
| `KIRO_CODE_EXECUTOR_IMAGE` | 默认 `python:3.12-slim`。部署时建议设置为已预拉取、同时包含 Python 3 和 Bash 的可信镜像 digest。 |
| `KIRO_CODE_EXECUTOR_DOCKER` | Docker CLI 路径，默认 `docker`。 |

服务使用 `--pull=never`，因此镜像须在启动前由运维预拉取；配置形式例如 `python@sha256:IMAGE_DIGEST`。普通 Python 标准库可用，不提供第三方依赖安装或网络下载。使用自定义镜像时须分别验证 `python3` 与 `bash`；缺少解释器会返回真实运行环境错误，不替换为其他解释器。

Docker CLI 使用独立的空配置目录和最小环境，不继承宿主 Docker context、`DOCKER_HOST`、HTTP 代理或其他宿主环境令牌。该临时配置目录在服务正常退出时删除。

## 请求协议

```http
POST /execute HTTP/1.1
Host: 127.0.0.1:12346
Authorization: Bearer SHARED_TOKEN
Content-Type: application/json

{"code":"print(sum(range(101)))"}
```

真实执行成功示例：

```json
{"stdout":"5050\n","stderr":"","return_code":0}
```

请求可选字段 `mode` 只允许精确小写字符串 `python` / `bash`；省略或空字符串均保留旧版 Python 行为，其他值在运行 Docker 前返回 HTTP 400 `invalid_mode`，非字符串返回 `invalid_request`。两种模式共用同一套资源限额与并发名额。

Bash 示例（代码仍通过 `code` 原样传入容器 stdin）：

```json
{"mode":"bash","code":"printf '%s\\n' \"$((19 * 23))\"\n"}
```

其真实输出为 `437\n`，退出码为 0。脚本可使用容器已有命令、管道、子进程和临时文件；不开放外网、宿主目录或跨请求状态。

Python 自身语法错误、异常、`sys.exit(n)` 或 Bash `exit n` 仍然属于真实执行结果，HTTP 200 返回实际 `return_code` 和实际输出；不会将退出码 125、126、127 一律误判为 Docker 失败。服务会读取容器的真实状态来区分执行结果与运行环境故障。

执行超时示例：

```json
{"stdout":"","stderr":"","return_code":-1,"error_code":"execution_timeout"}
```

`return_code: -1` 表示服务没有获得可信的解释器退出状态，不表示脚本实际返回了 -1。容器 OOM 退出会保留实际退出码，并附带 `memory_limit_exceeded`。Docker 初始化、状态确认或清理失败均明确返回错误，不回传原始宿主路径等 Docker 诊断。

## 固定执行边界

- 原始 HTTP body 最大 **96 KiB**；解码后的代码最大 **64 KiB**。
- `stdout`、`stderr` 各最多保存 **16 KiB**；任一输出超过限额就终止执行，保留限额以内的前缀并返回 `output_limit_exceeded`。
- 限额按原始字节计数。JSON 对控制字符进行转义，完整响应可能大于 32 KiB；调用端完整响应上限宜至少 **256 KiB**。
- 每次执行最长 **10 秒**；客户端取消或服务关闭也会取消运行。
- 同时最多 **2 个执行任务**，清理期间仍占用执行名额。没有等待队列，其余请求直接返回 HTTP 429。
- 容器名使用密码学随机值，每次执行重新创建，不跨请求保留解释器状态或文件。

代码通过 stdin 输入，Python 命令固定为：

```text
python3 -I -S -u -
```

Bash 命令固定为：

```text
bash --noprofile --norc -p -s --
```

`-p` 忽略 `BASH_ENV` / `ENV` 和导出的 shell 函数；容器用户仍为 UID/GID 65534，无附加能力，不提升容器或宿主权限。脚本内容不拼接进 `-c`、Docker 参数或宿主 shell 命令。

每个容器固定使用以下隔离设置：

```text
--network none
--read-only
--user 65534:65534
--cap-drop ALL
--security-opt no-new-privileges
--pids-limit 64
--memory 128m
--memory-swap 128m
--cpus 0.5
--ulimit nofile=64:64
--tmpfs /tmp:rw,noexec,nosuid,size=16m
--workdir /tmp
--log-driver none
```

服务不向容器挂载宿主目录或 Docker socket，不传入宿主环境变量；标准输出和错误输出通过受限内存缓冲区收集。`/tmp` 是每次执行独立的临时文件系统；根文件系统只读，临时目录不允许执行文件。

## 超时与清理

执行流程为 `docker run` → `docker inspect` 确认真实状态 → `docker rm -f`。容器不使用自动删除选项，以便读取真实退出状态，所有执行路径都由服务负责最终强制删除。

超时、客户端断开、输出超限和服务停止时，CLI 被取消后仍会以**不依赖原请求的独立清理 context** 执行 `docker rm -f`。清理自身也有短超时。若 Docker daemon 不可达或拒绝清理，响应会明确附带清理失败信息，而不宣称任务已正常完成。

## 状态与错误码

所有响应都保留 `stdout`、`stderr`、`return_code` 三个字段；错误时增加 `error_code`。

| HTTP | `error_code` | 含义 |
| --- | --- | --- |
| 200 | 无 | 已确认的真实 Python / Bash 退出结果，退出码可为非零。 |
| 400 | `invalid_request` | JSON 格式错误、缺少代码字符串、未知字段或额外 JSON 对象。 |
| 400 | `invalid_mode` | 请求的解释器不在 `python` / `bash` 枚举中。 |
| 401 | `unauthorized` | 缺少或不匹配的 Bearer token。 |
| 413 | `request_too_large` / `code_too_large` | 请求体或代码超限。 |
| 422 | `output_limit_exceeded` / `memory_limit_exceeded` | 输出超限或容器 OOM。 |
| 429 | `executor_busy` | 两个执行名额都已占用。 |
| 408 | `execution_cancelled` | 客户端取消或服务关闭。客户端断开时通常收不到响应，但清理仍执行。 |
| 504 | `execution_timeout` | 执行超过 10 秒。 |
| 502 / 503 | `runtime_error` / `runtime_unavailable` | Docker 或容器状态确认失败。 |
| 502 | `cleanup_failed` | 真实执行结果已经取得，但容器清理失败。 |
| 503 | `executor_stopping` | 服务正在停止，不再接收执行任务。 |

未知路径返回 404，非 POST 的 `/execute` 返回 405，响应同样采用结构化 JSON。

## 测试与部署验收

单元测试使用注入 Runner 和 `httptest`，覆盖令牌校验、真实回环绑定、模式白名单、两模式固定 Docker 参数和原样 stdin、请求与代码限额、真实退出码、输出/内存限额、共享并发 429、超时、HTTP 客户端取消、服务关闭，以及全部失败路径的独立清理。

部署验收应使用真实 Docker daemon 验证：标准库计算、非零退出、文件写入边界、无外网、无限循环超时、大输出终止，以及执行完成后的容器列表。不要将注入 Runner 的单元测试结果当作真实容器执行结果。

提供需显式启用的真实 Docker 集成测试，不会在常规单元测试中启动容器：

```bash
KIRO_CODE_EXECUTOR_INTEGRATION=1 \
KIRO_CODE_EXECUTOR_IMAGE=python@sha256:IMAGE_DIGEST \
go test -count=1 -run '^TestRealDockerModes' -v ./codeexec
```

该测试使用临时 HTTP 测试服务器与随机容器名，不连接生产 Worker；验证 Python 标准库、Bash 管道与子进程、非零退出、隔离边界、输出限额、超时与清理。镜像必须已存在。测试不下载依赖或启动持久服务。

主程序和该 Worker 可以分别升级或回退。关闭 Worker 会让调用方收到明确执行服务错误，不会自动退回宿主解释器执行。

生产 systemd 服务还应在 `ExecStartPre` 与 `ExecStopPost` 回收带有 `kiro.code-executor=true` 标签的本项目执行容器。普通 context 清理覆盖正常退出，但进程被 SIGKILL 或崩溃时没有机会运行 defer；生命周期回收负责这一边界。回收命令应验证容器 ID、限定标签并设置短超时，不触碰其他业务容器。部署验收还须测试执行中强杀 Worker、自动重启、残留容器归零及后续执行恢复。

主程序使用 `KIRO_CODE_EXECUTOR_URL`（字面回环地址）及同名 token-file 环境变量连接 Worker。可以通过 systemd `LoadCredential` 分别将凭据交给主程序和 Worker，无需让主程序读取或访问 Docker socket。API 密钥与宿主配置不传入执行容器。

Anthropic `code_execution_20250522` 对接本 Worker 的 Python 模式；`code_execution_20250825` 的 `bash_code_execution` 子工具将 `input.command` 原样交给 Bash 模式，其 `stdout`、`stderr` 和退出状态来自实际容器运行。现代工具的声明与响应类型参见[官方代码执行工具文档](https://platform.claude.com/docs/en/agents-and-tools/tool-use/code-execution-tool)。每次执行使用新容器，不提供持久 REPL、Files API、文件编辑器子工具或跨请求容器复用；不根据检测网站或提示词生成固定结果。
