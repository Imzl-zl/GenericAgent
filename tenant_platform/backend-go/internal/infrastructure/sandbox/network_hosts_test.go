package sandbox

import (
	"context"
	"strings"
	"testing"
)

func TestRunnerNetworkHostsGathersNamesAndAliases(t *testing.T) {
	runner := &fakeRunner{scripted: []fakeRunResult{
		{stdout: `[{"Containers":{"a":{"Name":"genericagent-llm-proxy-1","IPv4Address":"172.26.0.2/16"},"b":{"Name":"genericagent-platform-1","IPv4Address":"172.26.0.3/16"}}}]`},
		{stdout: `{"NetworkSettings":{"Networks":{"runner-control":{"Aliases":["genericagent-llm-proxy-1","llm-proxy"]}}}}`},
		{stdout: `{"NetworkSettings":{"Networks":{"runner-control":{"Aliases":["genericagent-platform-1","platform"]}}}}`},
	}}
	cli := &DockerCLI{cfg: validConfig(), runner: runner}

	hosts, err := cli.runnerNetworkHosts(context.Background(), "runner-control")
	if err != nil {
		t.Fatalf("runnerNetworkHosts: %v", err)
	}
	want := []string{
		"genericagent-llm-proxy-1:172.26.0.2",
		"genericagent-platform-1:172.26.0.3",
		"llm-proxy:172.26.0.2",
		"platform:172.26.0.3",
	}
	got := strings.Join(hosts, " ")
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("hosts missing %q: %v", w, hosts)
		}
	}
	if len(hosts) != len(want) {
		t.Fatalf("hosts = %v, want %v", hosts, want)
	}
}

func TestRunnerNetworkHostsSkipsBrokenInspect(t *testing.T) {
	// 逐容器 inspect 失败(exit 1)时, 容器名条目仍保留(别名缺失不阻断)。
	runner := &fakeRunner{scripted: []fakeRunResult{
		{stdout: `[{"Containers":{"a":{"Name":"genericagent-platform-1","IPv4Address":"172.26.0.3/16"}}}]`},
		{stderr: "inspect failed", code: 1},
	}}
	cli := &DockerCLI{cfg: validConfig(), runner: runner}

	hosts, err := cli.runnerNetworkHosts(context.Background(), "runner-control")
	if err != nil {
		t.Fatalf("runnerNetworkHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0] != "genericagent-platform-1:172.26.0.3" {
		t.Fatalf("hosts = %v", hosts)
	}
}

func TestRunnerNetworkHostsFailsOnBadNetworkInspect(t *testing.T) {
	runner := &fakeRunner{scripted: []fakeRunResult{
		{stderr: "no such network", code: 1},
	}}
	cli := &DockerCLI{cfg: validConfig(), runner: runner}
	if _, err := cli.runnerNetworkHosts(context.Background(), "missing-net"); err == nil {
		t.Fatal("missing network must fail closed")
	}
}
