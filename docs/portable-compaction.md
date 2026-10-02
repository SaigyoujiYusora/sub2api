# Portable Responses 压缩

GPT 保持原生远程压缩；非 GPT 可使用普通文本摘要。切换到非 GPT 时，Sub2API 可以按需调用 GPT-6 Luna，把原生加密包整理为明文交接摘要。

## 默认配置

```yaml
gateway:
  portable_conversion_model: gpt-6-luna
  portable_summary_model_mapping: {}
```

这些是本分支的新默认值；替换二进制并重启 Sub2API 后生效，不要求修改 Codex。常规压缩默认由原模型生成摘要，没有预设 Grok 或 DeepSeek 到 Luna 的生产映射。

Grok 的常规压缩模型可在账号新增、编辑和批量编辑界面的“Compact 专属模型映射”中指定，例如 `grok-* → gpt-6-luna`。清空账号映射且没有配置全局摘要映射时，恢复由 Grok 自己生成摘要。分组必须允许所选 GPT 文本模型并有支持它的 OpenAI Responses 账号。该账号映射独立于“GPT 加密包转明文”使用的 `portable_conversion_model`。

`portable_conversion_model` 控制非 GPT 收到原生包时使用的普通推理模型；设为空字符串可关闭自动转换。`portable_summary_model_mapping` 控制非 GPT 显式压缩请求的摘要模型；账号已有的 `compact_model_mapping` 匹配规则优先，未匹配的非 GPT 继续使用自身模型生成摘要。GPT 的原生压缩仍沿用原有映射逻辑。

## 处理方式

- OpenAI 账号映射到受支持的 `gpt-`、`chatgpt-`、o 系模型或专用审核模型 `codex-auto-review` 时，使用上游原生远程压缩，也允许重放原生加密历史。判定基于映射后的模型，不基于客户端传入的别名；图像模型、`gpt-oss` 等排除项不走此路径。
- 非 GPT 通过 HTTP `POST /v1/responses` 提交 `compaction_trigger`，或调用旧式 `POST /v1/responses/compact` 时，Sub2API 先解析用户配置的摘要模型映射。配置了跨模型映射时，调用所选模型的普通 `/v1/responses` 推理，原目标模型不再执行另一笔推理；未配置则继续使用自身模型。摘要请求移除 `compaction_trigger`、`context_management`、`background` 和工具配置，使用 `reasoning.effort=low`、`max_output_tokens=4096`。
- 摘要以 `sub2api:compact:v1:` 加 base64url 编码的 JSON 保存到响应的 `output[].encrypted_content`。包内包含版本、模型和摘要。Base64url 只是编码，不是加密；字段名 `encrypted_content` 不代表内容已加密。
- 只有真实 provider 正常终止且正文状态为 `completed` 时才发布便携包；截断、拒绝或其他未成功结果会返回带类型的错误，并保留已产生的 usage。

## 重放边界

- 重放时，便携摘要会展开为 `role=user` 的普通消息，正文放在 `<conversation_summary>` 标记中。展开发生在提供方转换之前，因此后续请求可以切换到支持原生压缩的 GPT 模型。
- 非 GPT 模型收到原生加密包时，网关按需调用 Luna 普通推理，要求写明文交接摘要。当前问题及压缩之后的新消息不进入缓存摘要，仍按原顺序交给目标模型。转换成功后重新调度原模型并复查余额和额度。转换失败时保留错误，不删除密文重试。
- 自家便携包不能与非空 `previous_response_id` 一起重放。WebSocket 可重放已有便携包；但非 GPT WebSocket 发起压缩会明确拒绝，需改用 HTTP `POST /v1/responses`。
- GPT 原生 compact 的内部重试不会跨到非 GPT 模式。媒体目标继续走原有媒体权限和协议路径，不交由文本摘要处理。

这些请求仍受现有 API key 访问控制。Luna 请求独立经过组模型白名单、复合/渠道映射、安全审查、账号调度和计费；它借用父用户槽位，父账号槽位和未使用预算会先释放，避免并发上限为 1 时互相等待。Luna 用量强制入账，父请求额度复查不重复计入 RPM。摘要是原对话历史的明文摘要，其敏感程度与原历史相同。

按需转换缓存按用户、API key、分组、密文 SHA-256、摘要模型和格式版本隔离，相同包的并发转换会合并。缓存仅驻留内存，最多 1024 项，24 小时过期；重启、过期或淘汰后可能重新调用一次 Luna。普通切换回合不会重写 Codex 本地旧包，因此网关缓存仍然必要。

旧包没有来源账号索引，使用用户/分组正常授权的 Luna 路由尝试，不保证所有来源或账号的密文都能读取。摘要只能整理包内仍保留的信息。跨模型生成与转换仅支持 HTTP；WS 仍只支持已有便携包重放。普通及 passthrough 摘要请求都会传递取消；已发生用量仍可能计费，取消后不再继续目标模型。

## 验证状态

2026-10-02，`TestPortableCompactionBridgeLiveLuna` 在测试配置中临时为 DS 指定 Luna，通过运行中的本地网关完成 5 次真实模型调用：DS 触发压缩 → Luna 明文摘要 → DS 回读，以及 Astra 原生包 → Luna 明文摘要 → DS 回读，两条路径均恢复全部 6 个任务字段。同包再次转换命中缓存、没有新增模型调用。DS→Luna 映射仅用于测试，不是生产默认。证据：`outputs/luna-portable-compaction-20261002/live-run-2`。测试没有部署候选二进制；完整 HTTP 重入的鉴权、可信客户端 IP、用户槽位借用，以及加密输出拒绝、缓存隔离、计费必达、RPM 不重复、取消和大整数保真由回归测试覆盖。

2026-10-02 修复了 `codex-auto-review` 被误判为非 GPT 而拒绝原生加密历史的问题，HTTP/WS 原生历史重放、映射到 GPT、映射到非 GPT 时拒绝的回归均通过。便携摘要请求移除 `background`，包括客户端传来的 `true`/`false`，避免 Grok 的 `Argument not supported: background`。显式启用的 `TestPortableCompactionLiveGrok` 使用修改后的 service 入口，通过运行中的本地网关调用真实 Grok；两次推理完成摘要和仅凭压缩包回读，含随机标记的 6 个任务字段全部恢复。证据保存在 `outputs/grok-compaction-fix-20261002/live`。本测试未替换运行中的 Sub2API。

此前使用未经修改的 Codex 0.159.2 和 fake server 通过了客户端契约 probe。这只验证了该客户端与 fake server 的契约，不代表真实模型验证或生产部署。共享的 cross-mode fallback guard 已完成并通过测试。Go 1.27.1 离线 `-tags unit` 验证中，相关 service/handler/apicompat 共 344 个顶层测试、597 个用例通过。Windows amd64 使用 `CGO_ENABLED=0 -tags embed` 后端编译成功，产物为 `F:/Games/Codex-Playground/outputs/sub2api-portable-compaction-verification.exe`。已完成测试和编译验证；本轮未安装、未重启服务，也未提交代码。
