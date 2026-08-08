// fakeserver 是测试用 mock stdio MCP server:
// 逐行读 stdin, 按 JSON-RPC 协议响应, 支持崩溃/延迟注入
// (--crash-after N: 处理 N 个请求后退出; --slow-ms N: 响应前延迟)。
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	crashAfter := flag.Int("crash-after", 0, "exit after handling N requests (0 = never)")
	slowMS := flag.Int("slow-ms", 0, "sleep N ms before each response")
	flag.Parse()

	handled := 0
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req map[string]any
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if *slowMS > 0 {
			time.Sleep(time.Duration(*slowMS) * time.Millisecond)
		}
		method, _ := req["method"].(string)
		id, hasID := req["id"]
		switch method {
		case "initialize":
			writeResp(id, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake-mcp", "version": "1.0"},
			})
		case "tools/list":
			writeResp(id, map[string]any{
				"tools": []map[string]any{{
					"name":        "echo",
					"description": "echo text back",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text": map[string]any{"type": "string"},
						},
					},
				}},
			})
		case "tools/call":
			params, _ := req["params"].(map[string]any)
			args, _ := params["arguments"].(map[string]any)
			text, _ := args["text"].(string)
			writeResp(id, map[string]any{
				"content": []map[string]any{{"type": "text", "text": "echo: " + text}},
			})
		case "notifications/initialized":
			// notification 无响应
		default:
			if hasID {
				writeResp(id, map[string]any{})
			}
		}
		handled++
		if *crashAfter > 0 && handled >= *crashAfter {
			fmt.Fprintln(os.Stderr, "fakeserver: crashing on demand")
			os.Exit(1)
		}
	}
}

func writeResp(id any, result map[string]any) {
	payload := map[string]any{"jsonrpc": "2.0", "result": result}
	if id != nil {
		payload["id"] = id
	}
	line, _ := json.Marshal(payload)
	fmt.Println(string(line))
}
