package upstream

import (
	"encoding/json"
	"os"
	"path/filepath"
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
//   - 文件不存在 / 解析失败 → 静默跳过（保持旧行为：独立命令本来就不依赖配置）；
//   - 读到了 proxies → 注入（与 server 同一条路径）。
//
// 查找顺序：显式路径 → 当前目录 config.json → 上一级 config.json（cmd 常在
// 仓库子目录里跑）。
func ConfigureProxiesFromConfigFile(explicit string) {
	for _, path := range configCandidates(explicit) {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var doc struct {
			Proxies map[string]string `json:"proxies"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		if len(doc.Proxies) > 0 {
			ConfigureProxies(doc.Proxies)
		}
		return
	}
}

func configCandidates(explicit string) []string {
	var out []string
	if p := strings.TrimSpace(explicit); p != "" {
		out = append(out, p)
	}
	out = append(out, "config.json")
	if wd, err := os.Getwd(); err == nil {
		out = append(out, filepath.Join(filepath.Dir(wd), "config.json"))
	}
	return out
}
