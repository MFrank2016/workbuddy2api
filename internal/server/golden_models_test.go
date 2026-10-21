package server

// golden_models_test.go 预迁移 golden：`/v1/models` 清单（W0 基线）。
//
// 为什么这个 golden 落在 upstream 的 testdata 目录
// ------------------------------------------------
// W0 的 core 侧 golden 只允许三类：chat 请求体、`/v1/models` 清单、URL + 出站头集合。
// 为了让「回滚前/后逐字比对」有一个**单一目录**（见 internal/upstream/testdata/golden/
// README.md），清单也放在那里，而不是另开一个 server/testdata/golden —— 两处各放一份
// 就等于有两个基线，比对时不知道该信哪个。
//
// 采集方式：真实 handler（`Handler.modelList()` → `/v1/models` 的响应体），
// 上游用既有的 fake（`newFakeUpstream`），零网络、零真实凭据。
//
// 确定性
// ------
//   - `resetModelsCache()` 清掉包级动态模型缓存（否则顺序依赖）；
//   - 只放**静态种子表里有**的模型（glm-5.2 / deepseek-v4.1-flash），
//     `ContextWindowListingV4` 走到第 2 级就返回，不会触发 models.dev 异步拉取；
//   - JSON 用 `json.MarshalIndent`（map 键天然排序）+ 末尾换行。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
)

// goldenModelsDir core 侧 golden 的统一目录（见文件头注释）。
var goldenModelsDir = filepath.Join("..", "upstream", "testdata", "golden")

const goldenModelsFile = "models_listing.json"

// goldenModelsUpstreamBody 上游 /v1/models 的固定响应（字段集对齐真实上游形态）。
const goldenModelsUpstreamBody = `{"code":0,"data":{"models":[
	{"id":"glm-5.2","name":"GLM-5.2","descriptionZh":"通用对话模型","credits":"x0.05","tags":["craft"],"vendor":"z-ai","maxInputTokens":1000000,"maxOutputTokens":131072,"supportsImages":true,"supportsReasoning":true,"supportsToolCall":true,"reasoning":{"effort":"high","summary":"auto","defaultEffort":"high","supportedEfforts":["low","high"]}},
	{"id":"deepseek-v4.1-flash","name":"DeepSeek V4.1 Flash","credits":"x0.02","maxInputTokens":1000000,"maxOutputTokens":384000,"supportsReasoning":true,"supportsToolCall":true,"reasoning":{"effort":"high","supportedEfforts":["low","high","max"],"defaultEffort":"high"}}
],"agents":[{"name":"cli","models":["glm-5.2","deepseek-v4.1-flash"]}]}}`

// goldenModelsListing 跑真实 modelList() 并归一化成 golden 文本。
func goldenModelsListing(t *testing.T) string {
	t.Helper()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, goldenModelsUpstreamBody, false
	})
	resetModelsCache()
	t.Cleanup(resetModelsCache)

	p := testPoolWith(&auth.Auth{UID: "golden-uid-0001", AccessToken: "golden-at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})

	raw, err := json.Marshal(map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
	if err != nil {
		t.Fatalf("marshal listing: %v", err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal listing: %v", err)
	}
	pretty, err := json.MarshalIndent(generic, "", "  ")
	if err != nil {
		t.Fatalf("indent listing: %v", err)
	}
	return string(pretty) + "\n"
}

// TestGoldenModelsListing `/v1/models` 清单逐字节快照。
func TestGoldenModelsListing(t *testing.T) {
	got := goldenModelsListing(t)

	path := filepath.Join(goldenModelsDir, goldenModelsFile)
	if os.Getenv("WB2A_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(goldenModelsDir, 0o755); err != nil {
			t.Fatalf("mkdir golden: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with WB2A_UPDATE_GOLDEN=1)", path, err)
	}
	if string(want) != got {
		t.Errorf("%s 与 golden 不一致 —— 若本次改动是有意的，用 WB2A_UPDATE_GOLDEN=1 重新生成并写清差异\n--- want ---\n%s\n--- got ---\n%s",
			goldenModelsFile, want, got)
	}
}

// TestGoldenModelsListingShape 清单形状自证：两个 cn: 模型、能力字段齐全。
func TestGoldenModelsListingShape(t *testing.T) {
	var listing struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(goldenModelsListing(t)), &listing); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if listing.Object != "list" {
		t.Errorf("object=%q want list", listing.Object)
	}
	if len(listing.Data) != 2 {
		t.Fatalf("data len=%d want 2", len(listing.Data))
	}
	for _, item := range listing.Data {
		id, _ := item["id"].(string)
		if id != "cn:glm-5.2" && id != "cn:deepseek-v4.1-flash" {
			t.Errorf("unexpected id %q (want cn: prefix)", id)
		}
		for _, field := range []string{"object", "created", "owned_by", "context_length"} {
			if _, ok := item[field]; !ok {
				t.Errorf("%s 缺字段 %s", id, field)
			}
		}
	}
}

// TestGoldenModelsListingDeterministic 两次采集完全相同（缓存复位后仍稳定）。
func TestGoldenModelsListingDeterministic(t *testing.T) {
	first := goldenModelsListing(t)
	second := goldenModelsListing(t)
	if first != second {
		t.Errorf("两次采集的清单不同:\n%s\n---\n%s", first, second)
	}
}
