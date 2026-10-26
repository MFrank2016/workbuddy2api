package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

type ChatExecOptions struct {
	Body     []byte
	Stream   bool
	Protocol string
	OnError  func(status int, code, msg, hint string)
}

type ChatExecution struct {
	RC         io.ReadCloser
	Account    *auth.Auth
	Release    func()
	Status     int
	Aggregated map[string]any
	BareModel  string
	Realm      string
	ModelRate  string
	ChatMeta   upstream.ChatMeta
	Stat       *chatStat
}

// chatPeek 是请求体里与选号/协议分流相关的少量字段。
type chatPeek struct {
	Stream bool   `json:"stream"`
	Model  string `json:"model"`
}

func peekChatBody(body []byte) (chatPeek, error) {
	var p chatPeek
	err := json.Unmarshal(body, &p)
	return p, err
}

// preparedChat 是预检后的协议无关请求上下文。
type preparedChat struct {
	Body          []byte
	Stream        bool
	Protocol      string
	OnError       func(status int, code, msg, hint string)
	Peek          chatPeek
	Realm         string
	BareModel     string
	ModelRate     string
	Profile       string
	CrossRealm    bool
	ChatMeta      upstream.ChatMeta
	StickyKey     string
	DeclaredTools map[string]bool
	ReqHasImage   bool
	KeyGroups     []string
}

// ChatExecError 是预检阶段产生的协议无关错误。
type ChatExecError struct {
	Status int
	Code   string
	Msg    string
	Hint   string
}

func (e *ChatExecError) Error() string { return e.Code + ": " + e.Msg }

// readBody 是各协议入口共用的请求体读取（含 server.max_body_mb 上限）。
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if h.cfg.MaxBodyBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxBodyBytes)
	}
	return io.ReadAll(r.Body)
}

// prepareChatRequest 执行读 body 之后的请求预检：模型/realm 解析、别名、
// 密钥白名单与 profile、图片策略/能力预检、models.hidden。成功时返回
// preparedChat，失败时返回协议无关的 ChatExecError。
func (h *Handler) prepareChatRequest(r *http.Request, opt ChatExecOptions) (*preparedChat, *ChatExecError) {
	body := opt.Body
	peek, err := peekChatBody(body)
	if err != nil {
		// 请求体必须是合法 JSON 对象：否则不该进入选号/轮转。旧行为是把坏 body 逐个
		// 账号转发给上游（实测 reqlog attempts=2），白耗配额；上游对重复非法请求会回
		// 403 account_fault(11140)，网关再按封禁禁用 → 客户端一个坏请求就能误禁健康号
		// （实测）。本地直接 400，零上游请求、不罚账号。
		return nil, &ChatExecError{
			Status: http.StatusBadRequest,
			Code:   "invalid_request",
			Msg:    "request body is not valid JSON: " + err.Error(),
		}
	}
	// 工具名名单：上游在没有 tools 的请求里会把工具调用吐成原生标记文本
	// （见 upstream/dsml.go），修复层用它做严格判定。名单来自「本请求声明的 tools」
	// 与「会话历史里出现过的工具名」两个来源——实测 tools 声明会在中转环节丢失，
	// 只认前者会让修复层在最需要它的场景下失效。
	// 在提示词/模型名改写之前取（那些改写不动 tools / messages[].tool_calls 子树）。
	declaredTools := upstream.ToolNameAllowlist(body)

	// realm 前缀解析（D6）：model 名可能带 "[realm:]" 前缀。剥出 realm + bareModel，
	// bareModel 用于选号/粘性/账本/出站 body 重写（前缀是网关侧路由协议，上游只认裸名）。
	// 裸名 → ("cn", 原串)，CN 现状零回归。
	realm, bareModel, realmExplicit := resolveModelDetail(peek.Model)
	modelRate := ""
	if h.cfg.Upstream != nil {
		modelRate = h.cfg.Upstream.ModelRate(realm, bareModel)
	}
	// 模型别名/后台任务重定向（upstream.model_aliases，默认空 = 不启用）：桌面客户端
	// 后台任务会硬编码付费模型，可在此改写到免费目标（省积分）。命中记日志；realm
	// 前缀保持，选号/账本/粘性都用改写后的目标模型。
	if target := matchModelAlias(h.cfg.ModelAliases, bareModel); target != "" && target != bareModel {
		log.Printf("alias: model %s -> %s", bareModel, target)
		bareModel = target
	}
	// 跨域回退（issue #135）：只有裸模型名 + 显式开启 pool.cross_domain_fallback 时生效。
	// 语义 = 「裸名不绑定单一域」：首选域（裸名默认 cn）选不到可用号（账号级冷却/熔断/
	// 在途占满，或该域账号对这个模型 11102 后进入模型级负缓存）时，改用另一域再选一次；
	// 显式 cn:/global: 前缀保持原语义，永不回退。
	crossRealm := false
	if h.cfg.Pool != nil {
		crossRealm = !realmExplicit && h.cfg.Pool.CrossDomainFallback()
	}

	// 密钥级模型白名单 + realm 绑定（吸收自 hub）：不匹配本地 400，零上游请求。
	key := authKeyFrom(r.Context())
	if key != nil {
		if key.Realm != "" && key.Realm != realm {
			return nil, &ChatExecError{
				Status: http.StatusBadRequest,
				Code:   "key_realm_mismatch",
				Msg:    "this api key is bound to realm " + key.Realm + ", but the requested model routes to " + realm,
			}
		}
		if len(key.Models) > 0 && !matchModelPatterns(key.Models, bareModel) {
			return nil, &ChatExecError{
				Status: http.StatusBadRequest,
				Code:   "model_not_allowed",
				Msg:    "model " + bareModel + " is not allowed for this api key",
			}
		}
	}
	// 产品 profile（契约 §1 / Todo 22）：越界/未知 header 在选号前 400；
	// profiles 未激活时恒为空，选号行为与迁移前逐字一致。
	profile, perr := h.resolveRequestProfile(r, key)
	if perr != nil {
		return nil, &ChatExecError{
			Status: http.StatusBadRequest,
			Code:   perr.Code,
			Msg:    perr.Msg,
		}
	}
	if profile != "" && auth.ProfileRealm(profile) != realm {
		return nil, &ChatExecError{
			Status: http.StatusBadRequest,
			Code:   "key_realm_mismatch",
			Msg: "profile " + profile + " routes to realm " + auth.ProfileRealm(profile) +
				", but the requested model routes to " + realm,
		}
	}
	if profile != "" {
		// 目录冷时异步补一次，供后续请求的能力预检使用；本次不阻塞。
		go h.warmProfileModels(profile)
	}

	// 图片策略与能力预检（Todo 12）：**读 body 之后、选号之前**。
	// 位置理由：两者都只依赖请求体 + 模型目录（不依赖账号），放在选号前意味着
	// 被拒请求零上游 chat 请求、不占租约、不罚任何账号。
	// 开关一律从 livecfg 取（面板热改），零值 = 迁移前行为（0/passthrough/false）。
	live := h.loadLive()
	body, imageCount, imgErr := upstream.ApplyImagePolicy(body, live.MaxImages, live.ImagePolicy)
	if imgErr != nil {
		var limitErr *upstream.ImagePolicyError
		if errors.As(imgErr, &limitErr) {
			return nil, &ChatExecError{
				Status: http.StatusRequestEntityTooLarge,
				Code:   "too_many_images",
				Msg: fmt.Sprintf("request contains %d images, which exceeds upstream.max_images=%d; reduce the number of images in messages",
					limitErr.ImageCount, limitErr.MaxImages),
			}
		}
		// 其他错误（当前不可达：ApplyImagePolicy 只在超限且 policy=error 时报错）
		// 不猜语义，按参数错误就地拒绝。
		return nil, &ChatExecError{
			Status: http.StatusBadRequest,
			Code:   "invalid_request",
			Msg:    imgErr.Error(),
		}
	}
	// 只在真的丢掉图片时打一行：策略开启但请求未超限是常态，不该刷日志。
	// 不二次 CountImages（body 已改写，再解析一次只为日志不值当）。
	if live.ImagePolicy == "truncate" && imageCount > live.MaxImages {
		log.Printf("[server] image policy: truncated %d -> %d (upstream.max_images=%d)", imageCount, live.MaxImages, live.MaxImages)
	}
	// 能力预检：仅当开关开、请求确实带图、且目录**显式**声明不支持（tri-state
	// Set=true + false）时拒绝。能力缺失（Set=false）一律 fail-open——上游目录
	// 大多数条目不下发 supportsImages，按「缺失 = 不支持」拦会把正常请求全挡掉。
	if live.CapabilityGuard && hasImagePart(body) {
		if cap := h.lookupModelCapability(realm, bareModel, profile); cap.InCatalog && cap.SupportsImagesSet && !cap.SupportsImages {
			return nil, &ChatExecError{
				Status: http.StatusBadRequest,
				Code:   "model_capability_mismatch",
				Msg:    "model " + bareModel + " does not support images",
				Hint:   upstream.NoImagesHint(bareModel),
			}
		}
	}
	// models.hidden 黑名单：点名调用直接拒绝，不打上游、不占选号。
	// 与 /v1/models 的列表过滤对称（只藏列表不够，知道名字仍可硬调）。
	if _, hide := h.loadLive().HiddenModels[realm+":"+bareModel]; hide {
		return nil, &ChatExecError{
			Status: http.StatusBadRequest,
			Code:   "model_unavailable",
			Msg:    "model is hidden by gateway configuration, try another model",
		}
	}

	return &preparedChat{
		Body:          body,
		Stream:        opt.Stream,
		Protocol:      opt.Protocol,
		OnError:       opt.OnError,
		Peek:          peek,
		Realm:         realm,
		BareModel:     bareModel,
		ModelRate:     modelRate,
		Profile:       profile,
		CrossRealm:    crossRealm,
		DeclaredTools: declaredTools,
	}, nil
}
