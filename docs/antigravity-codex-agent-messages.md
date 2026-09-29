# Codex agent messages through Antigravity

Codex multi-agent requests can carry the task body as a plaintext string in an
`agent_message.content[]` block named `encrypted_content`. The ordinary
Responses-to-Anthropic content whitelist does not recognize that block. A child
can therefore receive the `Payload:` envelope without its task even though the
request succeeds.

The Antigravity Responses entry point normalizes these delegation items before
converting the request to Anthropic and then Gemini:

- `agent_message` becomes a `message` with `role: user`.
- Its string-valued `encrypted_content` blocks become `input_text` blocks.
- Other input items, reasoning fields, tool results, and the original request
  buffer remain unchanged. Ordinary requests without agent messages are no-ops.

The two normalization functions are copied from CLIProxyAPI commit
`2eb8dd11d2480c5fd8bc8f2796cec6af534bc3b6`, with the upstream MIT license in
`backend/internal/pkg/apicompat/CPA-MIT-LICENSE`. This is protocol normalization,
not decryption; the caller must already provide readable task text.

The hook is limited to Antigravity's Responses entry point. OpenAI, Chat
Completions, billing identity, account selection, and original request metadata
retain their existing paths.

Regression tests exercise the complete Responses → Anthropic → Gemini
conversion: the original path loses first/follow-up task markers, while the
normalized path preserves both. They also check idempotence and preservation of
unrelated fields. These local conversion tests do not establish live upstream
availability; validate new child-agent conversations after deployment.
