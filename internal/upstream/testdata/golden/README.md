# 预迁移 golden（core 侧，W0 基线）

core 只服务 **chat** 与 `/v1/models`（不服务 Responses / Anthropic），所以这里的
golden 也只有三类：

| 文件 | 内容 | 采集点 |
|---|---|---|
| `chat_request_body.json` | 实际发往上游的 chat 请求体 | `upstream.Client.ChatStream`（`internal/upstream/golden_test.go`） |
| `chat_url_headers.txt` | 出站 URL + 完整头集合 | 同上（捕获 `RoundTripper` 收到的请求） |
| `models_listing.json` | `/v1/models` 响应 JSON | `server.Handler.modelList()`（`internal/server/golden_models_test.go`） |

`models_listing.json` 由 `internal/server` 的测试写入本目录，是为了让**整个 W0 基线
只有一个目录**：两处各放一份等于有两个基线，比对时不知道该信哪个。

## 归一化规则

golden 必须逐字节可复现，否则「跑两次 diff」的证明没有意义。

1. JSON：`json.MarshalIndent`（`encoding/json` 对 map 的键**天然排序**）+ 末尾换行。
2. 头集合：按键名排序，渲染成 `name: value`（名字小写）。同名多值逐行输出。
3. **消息级 ID 打码**：`X-Conversation-Message-ID` / `X-Request-ID` / `X-B3-SpanId`
   由 `session.NewMessageID()`（`crypto/rand`）每次新生成，是本次请求的消息标识、
   不属于协议形状 → 统一写成 `<message-id>`。
   会话级 ID（`X-Conversation-ID` / `X-Conversation-Request-ID` / `X-Root-Request-ID` /
   `X-Trace-ID` / `X-B3-TraceId`）由测试固定传入，**保持原值** —— 否则 golden 就
   证明不了「会话头族确实发出去了」。
4. 账号与凭据是**合成值**：`golden-access-token` / `golden-uid-0001` /
   `golden-enterprise`，不含任何真实 token 或真实账号目录。
5. 模型清单只放**静态种子表里有**的模型（`glm-5.2` / `deepseek-v4.1-flash`），
   查找链在第 2 级就返回，不会触发 models.dev 的异步网络拉取。

## 重新生成

```bash
cd /root/programming/github/workbuddy2api
export PATH=$PATH:/usr/local/go/bin
WB2A_UPDATE_GOLDEN=1 go test ./internal/upstream/ ./internal/server/ -run 'TestGolden'
```

**只在确认行为变更是有意的时候重新生成**，并把 diff 写进提交说明。
