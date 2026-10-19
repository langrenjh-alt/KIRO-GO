# 请求兼容与输入预算

## `thinking.display` 兼容

`thinking.type` 和 `thinking.budget_tokens` 继续按原规则校验。`display` 是可选的响应展示偏好，不改变上游是否生成推理：

- `summarized`：使用现有 thinking 展示。
- `omitted`：隐藏推理正文，保留正常文本、工具调用和原有签名续传行为。
- `updates`：兼容客户端的新展示值。Kiro 未区分独立进度更新块，因此本项目隐藏推理正文，不伪造进度更新。
- 其他字符串：忽略该展示偏好，沿用默认展示行为，不因未知字符串返回 400。
- 非字符串：仍由 JSON 类型校验返回请求错误。

展示值会先去除首尾空白并按大小写不敏感匹配。参见 [Anthropic thinking 文档](https://platform.claude.com/docs/en/build-with-claude/thinking)。

## 大量工具导致的固定输入压力

请求同时受到两个独立预算约束：

| 环境变量 | 默认值 | 含义 |
| --- | --- | --- |
| `KIRO_MAX_RAW_BODY_BYTES` | `921600`（900 KiB） | 本项目的本地请求体字节预算，不是模型 token 上下文大小 |
| `KIRO_INPUT_TOKEN_RESERVE` | `8192` | 从该模型输入 token 上限中预留的容量 |

例如，249 个工具的定义为 `tools_bytes=201715`、`schema_bytes=758820` 时，仅工具部分已有 960535 字节，超过本地字节预算。即使删除全部可裁剪历史，剩余 token 估算尚低于 1M，仍可能触发字节预算错误。

预检现在按真正发送 HTTP 请求的 JSON 序列化方式计算字节数：不额外转义 `<`、`>`、`&`。此前使用不同编码器，含有这些字符的请求会被高估。

### 处理顺序

1. 已满足字节和 token 预算的请求保持不变。
2. 沿用现有图片缩减和按完整会话边界裁剪旧历史的流程；系统前导消息、当前输入、当前工具结果及其对应的工具调用保持受保护。
3. 只有历史裁剪仍不能解决固定输入过大时，才尝试工具文档缩减，并重新搜索尽量保留近期历史的方案。
4. 首先规范化描述的 CRLF 换行，移除 schema 的展示注释 `title`、`examples`、`$comment`；完整描述仍保留。
5. 若仍超预算，依次尝试每个描述 4096、2048、1024、512、256 字节的上限。描述保留开头及结尾，UTF-8 字符保持完整，省略处带有明确标记；Markdown 内部空白和代码缩进不会被压平。
6. 仅在候选方案满足两个预算后应用工具文档和历史修改。没有合适候选时返回本地请求错误；该错误不触发账号故障冷却或账号轮换重试。

工具文档缩减会减少模型所见的自然语言说明和示例，不等同于语义完全无损。它不会删除工具、重命名工具，或删除 schema 的验证条件、`default`、`enum`、`const`、`$ref`、`$defs` 等功能数据。JSON 数字在复制工具 schema 时保留原表示，避免大整数经过 `float64` 损失精度。

schema 遍历只识别 schema 所在位置，不对任意 JSON 对象递归删除字段。因此，真实参数名 `description`、`title`、`examples`、`default`，以及 `const`/`enum`/`default` 中的同名实例数据均保留。schema 的注释与验证词汇在 [JSON Schema 规范](https://json-schema.org/draft/2020-12) 中有明确区分。

工具 schema 如果通过引用指向注释子树（例如 `#/examples/0`），会保留该 schema 的注释和描述，不破坏引用目标。无法从 JSON Pointer 确定目标的锚点引用也保守保留；任何外部或相对路径引用都会保留整组工具的 schema 文档，以保护潜在的跨 schema 或嵌套 `$id` 引用目标。工具级描述仍可参与缩减。

如果验证约束或不可裁剪的当前输入本身已超过预算，请求仍返回 400，并携带不含提示词正文的数字诊断；本次修复不会通过删除验证约束使请求勉强通过。

### 运维验证

`[KiroPreflight]` 日志及超限错误增加数字字段：

- `tool_context_bytes_saved`
- `compacted_tool_descriptions`
- `compacted_schema_descriptions`
- `removed_schema_annotations`

可运行针对性回归：

```powershell
go test ./proxy -run TestRequestCompatReview -count=1 -v
go test ./proxy -run TestClaudeThinkingDisplay -count=1 -v
go test ./proxy -run TestKiroPayloadPreflight -count=1 -v
go test ./proxy -run TestHTTPProtocolsMapFixedPayloadPreflight -count=1 -v
```

独立 249 工具回归夹具同时检查完整工具数量、受保护上下文、schema 验证/默认数据、来源对象隔离，以及重复预检的幂等性；另一夹具确认超大的 `const` 验证值仍被保留并正确拒绝。
