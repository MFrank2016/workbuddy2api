package upstream

import (
	"encoding/json"
	"log"
	"os"
	"strings"
)

// ConfigureProxiesFromConfigFile 从上游 config.json 读取 `proxies` 段并注入线路表。
//
// 为什么独立命令（signin / credit / trial / activity）也需要这一步：
// 它们各自构造 upstream.Client 直接发请求，但**不经过 cmd/server 的 main**，
// 于是不会调用 ConfigureProxies。而账号文件里可能已经绑了线路名 —— 不加载线路表
// 的话，这些命令会对每个账号报「出口线路不可用」，整批任务全红。
//
// 行为约定：
//   - 文件不存在 → 静默跳过（独立命令本来就不依赖配置）；
//   - 文件存在但读不了 / 解析失败 → 打一条 WARN 再跳过（不能静默：那会让所有
//     绑了线路的账号报「上游没有配置任何线路」，而真正原因是这份配置）；
//   - 读到了 proxies → 注入，并打一行「加载了几条、来自哪个文件」便于对账。
//
// 查找范围**只有**显式路径或当前工作目录的 config.json —— 不再向上级目录探测：
// 那可能读到无关项目的配置，静默把账号流量导到别的代理上。
func ConfigureProxiesFromConfigFile(explicit string) {
	path := strings.TrimSpace(explicit)
	if path == "" {
		path = "config.json"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		// 文件不存在是正常形态（这些命令只读 auths/）。
		return
	}
	var doc struct {
		Proxies map[string]string `json:"proxies"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.Printf("WARN: 出口线路：读取 %s 失败，线路表按空处理：%v", path, err)
		return
	}
	if len(doc.Proxies) == 0 {
		return
	}
	ConfigureProxies(doc.Proxies)
	log.Printf("出口线路已加载：%d 条（来自 %s）", len(RouteNames()), path)
}
