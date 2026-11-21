# Tool Schema Multimodal Compatibility

Some Responses providers reject a text request with HTTP 400,
`multimodal_forbidden`, and `audio/video content is not allowed` when a function
parameter is named `audio` or `video`. Browser fingerprint settings are one
example; these declarations are not media input.

The HTTP Responses gateway automatically retries this exact rejection once on
the same account. It aliases the affected function's field names to
`__s2a_audio` / `__s2a_video`, preserves the schema constraints and rewrites
historical function arguments. The response adapter restores the original names
before returning calls to the client. This applies to both normal forwarding and
passthrough, including streaming and JSON responses. It is not tied to a
particular account, model, domain, or tool name. Newly added accounts inherit the
behavior without configuration.

Only affected argument deltas are buffered (up to 4 MiB per call); normal text
continues streaming. Arguments, done items and terminal response output use the
same original field names. Namespaces and existing custom-tool adapters remain
reversible. The first request is unchanged; no provider capability is cached.

Actual audio/video content, unrelated errors, alias collisions, regex property
constraints, remote references and dynamic references do not trigger this
retry. Unsupported schemas or a second rejection retain the existing error
handling. The compatibility path does not remove fields, switch models, mark
an account unhealthy, or introduce a database migration. It does not add retry
handling for WebSocket upstreams or HTTP-200 stream errors.

Regression checks:

```sh
cd backend
go test ./internal/pkg/apicompat ./internal/service \
  -run 'Test.*(ToolFieldAliases|MultimodalSchemaCompat|ResponsesClientTool)'
```

The gateway logs `Retrying tool schema multimodal false positive once` with the
account ID. Logs do not include request contents or credentials. Verification
must check the selected account and actual upstream model, since a configured
model mapping can otherwise conceal the original provider failure.
