# CCTest 兼容性修复与分层复测（v1.1.8）

更新日期：2026-10-19（北京时间）。本文记录 v1.1.8 现代 Bash 兼容修复，区分已部署基线、候选版本验收、通用回归、前置网关修复和正式检测结果；不将其中任意一项替代其他项。**v1.1.8 已部署；最终正式 CCTest 评分待复测，不将接口回归替代站点评分。**

## 当前已验证状态

| 层次 | 状态与边界 |
| --- | --- |
| 已部署版本 | **v1.1.8 已实际部署**；保留旧格式与 JSON Schema 校验，按版本真实执行 Python/Bash。 |
| v1.1.8 执行协议 | 按声明版本区分 **20250522 Python / 20250825 Bash**；现代 Bash 已通过真实 Docker 执行、退出码、隔离、限额、超时、取消与逐容器清理验收。主程序与 Worker 健康门禁通过，正式评分待复测。 |
| 通用协议夹具 | 历史 Opus 4.8 的结构化格式、客户端强制工具、原生 Python 执行各流式/非流式共 **8/8 通过**；前置修补后 Opus 5.5 公网通用夹具也为 **8/8**。这是指定夹具的结果，不是 CCTest 评分。 |
| 最近一次正式 Opus 5.5 检测 | v1.1.8 修复前的报告总分 **82**，结构化输出 **0**、工具调用 **0**；模型归因项通过，检测站报告约 **93%** 的归因概率。该历史报告不代表 v1.1.8 的最终结果。 |
| 公网阻断历史与修复 | 此前指定工具的 Opus 5.5 请求被前置 Sub2API 静态能力 guard 返回 **400**，相关 thinking 参数也受影响。该前置原因已通过下述最小补丁与单通道开关处理，不再将历史 400 作为当前全部失败的解释。 |
| 前置最小补丁 | 已保存于 [`ops/patches/sub2api-passthrough-capabilities.patch`](../ops/patches/sub2api-passthrough-capabilities.patch)，前置 **0.2.8-fork-kiro-compat-20261019 已部署**，保护的依赖、迁移和嵌入资源 636 项哈希未变。 |
| 通道开关 | 本项目对应前置通道的 passthrough flag 已经通过正常管理 API 单独启用；其他 extra、凭据、分组和状态复读一致。 |

**通用 8 项回归通过 ≠ CCTest 评分通过。** 模型、参数组合、工具定义和网关路径需要分别记录；私有评分规则保持未知，不从得分反推其判定条件。尤其不根据内部评分键 `server_tool` 猜测实际工具类型；本次工具版本来自专用测试请求的实际观察。归因概率是检测站给出的统计结果，不是独立的模型身份认证。

## v1.1.8 的功能与兼容边界

### 结构化输出

- 接受 `output_format` 作为 `output_config.format` 的旧字段别名；同时存在时，非空的 `output_config.format` 优先，`output_config.effort` 保留。
- 请求中的无效或不支持的 JSON Schema 在生成前返回 400。外部 schema 引用不通过网络加载。
- 正常最终答案先规范化常见 Markdown/说明包装，再校验 JSON 和 schema；结构化流式响应也在校验前保持私有缓冲，不先泄露不可撤回的无效 JSON。
- 失配只允许一次有界格式纠正；持续失配返回真实 502，而不是生成固定合格对象。
- 真正的工具调用不是最终结构化答案。保留工具调用、拒绝、`max_tokens`、`pause_turn`、上下文限制及取消语义；不把截断或拒绝改造成成功。
- JSON 字符串中的字面 thinking 标签保留；结构化正文与推理块分开。格式纠正和失败链路保留已消费使用量，成功不重复结算。

### 按工具版本真实执行 Python / Bash

v1.1.7 曾将 `code_execution_20250522` / `code_execution_20250825` 都走独立 Python 兼容路径。实际 CCTest 请求声明的是 20250825；v1.1.8 按工具版本分流，保留两套实际执行协议：

| 客户端声明 | 实际调用与返回块 | Worker 真实执行 |
| --- | --- | --- |
| `code_execution_20250522`，名称 `code_execution` | `code_execution` / `code_execution_tool_result` | 原始 Python 代码通过 stdin 交给容器内 `python3 -I -S -u -`。 |
| `code_execution_20250825`，名称 `code_execution` | `bash_code_execution`，输入 `command`；返回 `bash_code_execution_tool_result`，内容为 `bash_code_execution_result` | 原始命令作为 `code`、显式 `mode: "bash"` 交给 Worker，在容器内执行固定命令 `bash --noprofile --norc -p -s --`；需要 Python 时由该 Bash 命令调用容器中的 Python。 |

主程序通过认证的回环 HTTP 接口调用独立 Worker；Worker 在受限 Docker 容器内执行实际代码，返回实际 `stdout`、`stderr`、`return_code`，再把执行结果交回模型生成最终答案。未执行代码时不会生成成功结果；Python/Bash 的实际非零退出与运行环境故障分别保留。

每次请求至多 3 次执行；每次新建容器，不提供持久 REPL、Files API、文件编辑器子工具或跨请求文件复用。两模式共享每次 10 秒、2 个并发名额、代码与输出上限；容器无网络、根目录只读，不挂载宿主文件或 Docker socket，不传入宿主凭据。未配置 Worker 时明确返回执行服务错误。完整配置、资源边界、取消与崩溃回收见 [独立 Python / Bash 执行 Worker](code-executor.md)。

v1.1.8 的真实 Docker 验收已覆盖通用 Bash 管道、Python 子进程、实际非零退出、隔离边界、输出限额、忽略 TERM 的脚本超时和 HTTP 取消，并逐一确认本测试创建的随机容器已删除。该验收是通用执行能力证据，不是针对检测题生成固定输出，也不等同于正式 CCTest 得分。

## 正式检测的实际请求与复现

仅观察专用测试 key 的请求后，已确认两个探测的输入（不是从评分名称猜测）：

- 结构化探测要求 `identity_platform`（枚举含 `claude_code`、`kiro` 等）和 `desc`，并询问真实平台身份。使用抓取的白名单输入复现，**HTTP 200、实际 JSON 可解析且符合该 schema，实际 `identity_platform` 为 `kiro`**；原抓取未保存 system 与工具描述，因此不将复现称为逐字节重放。最近一次正式报告的该项仍为 **0 分**；这是独立观察，不能将其等同为 JSON/schema 无效。没有证据说明私有评分如何处理该身份值，不作因果推断，也不为改变得分将身份硬编码为 `claude_code`、`kiro` 或其他固定值。
- 工具探测实际声明 `code_execution_20250825`，要求真实执行 Python 输出指定文本。v1.1.7 已真实执行（stdout 正确、return_code=0），但返回了旧版 `code_execution` / `code_execution_tool_result`。v1.1.8 改为 [官方现代协议](https://platform.claude.com/docs/en/agents-and-tools/tool-use/code-execution-tool)：`bash_code_execution` / `bash_code_execution_tool_result`，并通过 worker 的显式 Bash 模式在同样受限的容器中执行原始命令。20250522 保持旧版 Python 协议。

### v1.1.8 公网最终回归

部署后待原有前置自动冷却自然到期，2026-10-19 17:36 后以 Opus 5.5 重测，通用接口 **8/8、均 HTTP200、退出0**。同一现代工具 payload 的 A/B 已完成：本项目返回现代 Bash 块并实际输出 `HELLO_CHECK\n`、return_code=0；参考站同 payload 返回现代块但实际执行不可用。两站结构化 JSON/schema 均有效，身份值分别为 `kiro` 与 `claude_code`。两项规范化输入 SHA-256 分别为 `59ac1c04f8822267c6bf4a7e14b98fd7ffc6629f47ff2fcb09758f30af36ac1d`、`a9b19658a2aecb055ddd6c5ecc2d9346c990fbe9ff2cb8d645849bc65108480e`。

实际部署主程序 SHA-256：`810c1f9d65ee39f2a624bc37c63297cc30eadb6708b7d4a22895ca176e0cce8f`；Worker：`c418530f29f837a470ec0114ae95dd34b8929a00b5303353d1b9dc97088a4b7f`。Linux全包race/vet及代理池/分页2项回归通过。上述结果不替代新版正式网站评分。

### 待最终更新的正式复测

**v1.1.8 最终 CCTest 复测尚未运行。** 发布后需补录实际主程序与 Worker 部署版本/哈希、复测北京时间、正式报告链接及各项实际得分。现有 82/0/0 是修复前的历史结果，不预先替换为通过；真实容器执行、JSON/schema 校验与网站评分分别记录，不推断私有评分规则，不硬编码身份或检测答案。

## 同模型 A/B 的额外观察

使用同一 `claude-opus-5-5` 和完全相同的白名单复现请求：参考站的结构化 JSON/schema 合格，返回 `identity_platform=claude_code`；本项目旧版复现同样合格，返回 `kiro`。这两个事实不证明检测站的私有评分条件。

参考站现代工具返回 `bash_code_execution` / `bash_code_execution_tool_result`，但实际 `stdout` 为空、`return_code=1`、`stderr` 明确执行不可用。因而“块类型兼容”与“执行成功”是两项不同的验收；本项目修复正确块类型，并保留真实执行，不生成伪成功结果。

参考站同模型通用 8 项矩阵为 2/8：新格式 2 项通过，旧格式不是合格 JSON，forced tool 被其前置返回 400，legacy 执行不可用。这不是该站正式 CCTest 得分，不从中否定或预断正式评分。

## 前置 Sub2API：最小能力委托补丁

修补基于已确认的原前置源码提交 **`6c69c9c18`**，生产前置二进制基线 SHA-256 为：

```text
d3adb5b0a807d60534725b031b78c132d838c68302f474e4043239ee84f3f7cf
```

补丁仅调整生成与 `/count_tokens` 两处能力校验的条件：

1. 仅当 Anthropic **API Key** 账号的 `extra.anthropic_passthrough` 为布尔值 **`true`** 时，将模型能力判断交给该账号配置的真实上游。
2. flag 缺失、`false`、字符串 `"true"`、其他账号类型或其他平台均保持原校验规则。
3. 不改变请求模型名，不将 forced tool 改成 auto，不删除或改写 thinking，不针对检测站题目或域名提供特制答案。
4. 生成请求与真实上游响应保持透传；`count_tokens` 沿用原有生成字段过滤，只改变能力验证委托位置。
5. 上游仍可真实拒绝请求。保留上游错误状态和错误正文，不因错误自动降级模型或参数。

该补丁不是“Opus 5.5 一律支持所有参数”的声明。前置只在运维明确选定兼容上游的通道委托验证，最终能力由实际上游决定。

### 分层部署验收步骤

- 以准确原源码构建补丁候选，保留原二进制、哈希、版本、服务配置和可执行回退入口；不以整体升级替代该最小修补。
- 管理 API 修改前读取并保留本项目单一通道的完整当前对象；只将 `extra.anthropic_passthrough` 改为布尔值 `true`，合并而不是覆盖其他 `extra` 字段。
- 修改后重新读取通道，逐字段比较；认证信息、模型映射、分组、调度、限额和其他通道均应不变。
- 在前置候选和该单一通道 flag 都生效后，分别验证直连 KIRO-GO 与公网链路，再运行新的正式 Opus 5.5 CCTest。
- 更新本文前，必须记录实际部署哈希、原始 HTTP 结果及正式报告结果；前置已部署、Opus 5.5 公网通用夹具已由 6/8 恢复为 8/8；正式 CCTest 两项仍须独立验证。

## 可复用的 curl 复测模板

以下使用 Bash、Python 3 和 curl。测试密钥由环境变量 `KIRO_TEST_KEY` 注入；示例不包含实际密钥或服务器凭据。密钥通过 curl 的标准输入配置传递，不放进 curl 参数；不要启用 shell `set -x` 或 curl trace。

```bash
export API_BASE_URL='https://YOUR_ENDPOINT'
export API_MODEL='claude-opus-5-5'
# 使用现有凭据注入方式设置 KIRO_TEST_KEY；不要把实际值写入本文或提交记录。
: "${KIRO_TEST_KEY:?请通过环境注入专用测试密钥}"
case "$KIRO_TEST_KEY" in
  *[!A-Za-z0-9._-]*) printf '%s\n' '测试密钥格式错误' >&2; exit 1 ;;
esac
umask 077
export RETEST_DIR
RETEST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/kiro-cctest-retest.XXXXXX")

python3 - <<'PY'
import copy, json, os, pathlib
root = pathlib.Path(os.environ['RETEST_DIR'])
base = {'model': os.environ['API_MODEL'], 'max_tokens': 2048}
schema = {'type': 'object', 'properties': {'answer': {'type': 'integer'}},
          'required': ['answer'], 'additionalProperties': False}
cases = {
    'schema': {
        'messages': [{'role': 'user', 'content': 'Return one JSON object containing answer=42.'}],
        'output_config': {'format': {'type': 'json_schema', 'schema': schema}},
    },
    'legacy-schema': {
        'messages': [{'role': 'user', 'content': 'Return one JSON object containing answer=42.'}],
        'output_format': {'type': 'json_schema', 'schema': schema},
    },
    'forced-tool': {
        'messages': [{'role': 'user', 'content': 'Call get_weather for Paris. The client will execute it; do not invent weather.'}],
        'tools': [{'name': 'get_weather', 'description': 'Client-side weather lookup.',
                   'input_schema': {'type': 'object', 'properties': {'city': {'type': 'string'}},
                                    'required': ['city'], 'additionalProperties': False}}],
        'tool_choice': {'type': 'tool', 'name': 'get_weather'},
    },
}
for name, extra in cases.items():
    for stream in (False, True):
        request = copy.deepcopy(base)
        request.update(copy.deepcopy(extra), stream=stream)
        suffix = 'stream' if stream else 'nonstream'
        (root / f'{name}-{suffix}.json').write_text(json.dumps(request), encoding='utf-8')
PY

api_post() {
  request_file=$1
  result_prefix=$2
  printf 'header = "x-api-key: %s"\n' "$KIRO_TEST_KEY" |
    curl --config - --silent --show-error --no-buffer \
      --request POST --url "${API_BASE_URL%/}/v1/messages" \
      --header 'content-type: application/json' \
      --header 'anthropic-version: 2023-06-01' \
      --data-binary "@$request_file" --output "$result_prefix.body" \
      --write-out 'http=%{http_code} content_type=%{content_type}\n' > "$result_prefix.meta"
}

for name in schema legacy-schema forced-tool; do
  for mode in nonstream stream; do
    api_post "$RETEST_DIR/$name-$mode.json" "$RETEST_DIR/$name-$mode"
  done
done
```

`curl` 的退出码表示传输是否成功，**不能把退出 0 当成 HTTP 200 或语义通过**。检查 `.meta` 的 HTTP 状态及 `.body` 的真实 JSON/SSE；不要将含会话内容或签名的原始结果提交仓库。

### 验收条件

- **非流式 schema**：HTTP 200；取 `content` 中 text 块的完整正文，解析后验证原请求 schema。不能只检查正文看起来像 JSON。
- **流式 schema**：HTTP 200 和 SSE；按内容块索引重建 `text_delta`，检查 `message_start` → 内容块 → `message_delta` → `message_stop` 生命周期，再验证重建正文的 schema。
- **强制工具**：HTTP 200；实际存在指定名称的 `tool_use`，输入是符合工具 schema 的 JSON；终止原因为 `tool_use`。流式按同一索引拼接 `input_json_delta.partial_json` 后再解析，不能把文本模拟调用当成功。
- **工具续轮**：将返回的工具调用与真实客户端结果加入下一轮，结果的 `tool_use_id` 必须准确引用该调用；再验证后续答案，不能伪造工具结果。
- **thinking 组合**：使用新的合成夹具分别加入 `thinking: {"type":"enabled","budget_tokens":1024}`、`disabled` 或 `adaptive`，保留原格式/forced tool 参数。确认前置完整转交或真实上游明确拒绝，而不是隐式改参。
- **原始错误保真**：用无效 schema、未知指定工具等负例确认真实 4xx；若真实上游因能力拒绝，仍应保留其错误。对于已提交的 SSE，检查 error 事件而不是追加伪成功结束。
- 对同一模型、同一请求分别测试内部上游与公网链路，判断拒绝发生在哪一层；不同模型的通过结果不可互换。

## 诊断与回滚原则

- 优先使用只记录匹配专用测试密钥请求的 `requests-only` 诊断；不保存原始认证头、pcap 或其他用户请求/响应。不能依据未见完整握手的中途 TCP 抓取推断响应归属。
- 更改前置 flag 和二进制分别保留快照。失败时恢复该单一通道原 flag/完整配置，原子恢复已校验哈希的前置基线并重启验证；不恢复整个数据库、不覆盖其他通道或当前运行数据。
- KIRO-GO、执行 Worker 与前置分别保留回退材料。回退前置不会撤销 KIRO-GO v1.1.8；应按本次部署事务回退相应主程序、Python/Bash Worker 与单通道 flag，Worker 的停止、凭据配置和容器回收按其独立回退入口处理。
- 回退后再次比较配置、健康状态及原始负例响应，确认旧行为确实恢复。正式检测未通过时保留“待验证/仍失败”状态，不以单元测试、人工夹具或模型改名代替正式结果。
