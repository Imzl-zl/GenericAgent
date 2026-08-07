package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
)

// networkMember 是 docker network inspect 输出的单个容器条目。
type networkMember struct {
	Name        string `json:"Name"`
	IPv4Address string `json:"IPv4Address"`
}

type networkInspectEntry struct {
	Containers map[string]networkMember `json:"Containers"`
}

type containerInspect struct {
	NetworkSettings struct {
		Networks map[string]struct {
			Aliases []string `json:"Aliases"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// runnerNetworkHosts 返回 docker network 上全部容器的 /etc/hosts 条目
// ("name:ip" 与网络别名 "alias:ip", 去重)。Runner 使用 runsc(gVisor)
// 运行时, 其 netstack 与 Docker 内嵌 DNS(127.0.0.11)不兼容(实测 UDP 查询
// 持续 EAI_AGAIN, runc 正常)——内部服务名(platform/llm-proxy)必须经
// /etc/hosts 静态解析。外部域名 Runner 一律经 Platform 代理访问
// (LLM/Sophub/MCP), 不需要出站 DNS, 无需上游解析器。
func (d *DockerCLI) runnerNetworkHosts(ctx context.Context, network string) ([]string, error) {
	stdout, _, exitCode, err := d.runner.Run(ctx, d.cfg.Binary, "network", "inspect", network)
	if err != nil {
		return nil, fmt.Errorf("docker network inspect: %w", err)
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("docker network inspect %s failed (exit %d)", network, exitCode)
	}
	var entries []networkInspectEntry
	if err := json.Unmarshal(stdout, &entries); err != nil {
		return nil, fmt.Errorf("parse docker network inspect: %w", err)
	}
	if len(entries) != 1 {
		return nil, fmt.Errorf("docker network inspect %s returned %d entries, want 1", network, len(entries))
	}

	hosts := make([]string, 0, len(entries[0].Containers)*2)
	seen := make(map[string]struct{}, len(entries[0].Containers)*2)
	add := func(name, ip string) {
		key := name + ":" + ip
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		hosts = append(hosts, key)
	}

	// 按容器名排序, 保证 hosts 条目顺序确定(便于测试与 diff)。
	members := make([]networkMember, 0, len(entries[0].Containers))
	for _, member := range entries[0].Containers {
		if strings.TrimSpace(member.Name) == "" {
			continue
		}
		members = append(members, member)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	for _, member := range members {
		ip, _, err := net.ParseCIDR(strings.TrimSpace(member.IPv4Address))
		if err != nil || ip == nil {
			continue
		}
		add(member.Name, ip.String())
		// compose 服务名是网络别名, 不在 network inspect 里——逐容器 inspect。
		aliasOut, _, aliasCode, aliasErr := d.runner.Run(ctx, d.cfg.Binary, "inspect", member.Name)
		if aliasErr != nil || aliasCode != 0 {
			continue // 容器名条目已足够; 别名缺失不阻断创建
		}
		// docker inspect 输出是 JSON 数组(即使单个容器), 与 network inspect 不同。
		var infos []containerInspect
		if err := json.Unmarshal(aliasOut, &infos); err != nil || len(infos) == 0 {
			continue
		}
		for _, alias := range infos[0].NetworkSettings.Networks[network].Aliases {
			if strings.TrimSpace(alias) != "" {
				add(alias, ip.String())
			}
		}
	}
	return hosts, nil
}
