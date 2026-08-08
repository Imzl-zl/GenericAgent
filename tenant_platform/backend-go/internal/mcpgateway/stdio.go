package mcpgateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// stdioProcess 是一个受管的 stdio MCP 子进程。
// MCP stdio 协议是单通道 request/response 串行: 所有请求经 reqMu 串行化。
// reader goroutine 持续消费 stdout, 行按序进入 lines channel;
// 请求方循环匹配 jsonrpc id(容忍进程输出非 JSON 行)。
type stdioProcess struct {
	def     Server
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lines   chan []byte
	done    chan struct{}
	reqMu   sync.Mutex
	lastUse atomic.Int64 // unix nano, 供空闲回收
	dead    atomic.Bool
	kill    sync.Once
}

var errProcessDead = errors.New("stdio process is dead")

// runJSONRPC 串行发送一条 JSON-RPC 并等待同 id 的响应。
// notification(true) 时只写不读。整体超时 = def.Timeout;
// 超时 kill 进程(挂死的进程不可信——响应流已被污染, 必须重建)。
func (p *stdioProcess) runJSONRPC(
	ctx context.Context, reqID any, method string, params map[string]any, notification bool,
) (map[string]any, error) {
	p.reqMu.Lock()
	defer p.reqMu.Unlock()
	if p.dead.Load() {
		return nil, errProcessDead
	}
	payload := map[string]any{"jsonrpc": "2.0", "method": method}
	if !notification {
		payload["id"] = reqID
	}
	if len(params) > 0 {
		payload["params"] = params
	}
	line, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal jsonrpc: %w", err)
	}
	if err := p.writeLine(ctx, append(line, '\n')); err != nil {
		p.markDead()
		return nil, fmt.Errorf("write to stdio process: %w", err)
	}
	if notification {
		p.touch()
		return nil, nil
	}
	timer := time.NewTimer(p.def.Timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			p.markDead()
			return nil, fmt.Errorf("await stdio response: %w", ctx.Err())
		case <-timer.C:
			p.markDead()
			return nil, fmt.Errorf("stdio response timeout after %s", p.def.Timeout)
		case raw, ok := <-p.lines:
			if !ok {
				p.markDead()
				return nil, errors.New("stdio process exited before responding")
			}
			msg := make(map[string]any)
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue // 非 JSON 行(进程日志)按协议忽略
			}
			msgID, hasID := msg["id"]
			if !hasID || !idsEqual(msgID, reqID) {
				continue
			}
			p.touch()
			return msg, nil
		}
	}
}

func (p *stdioProcess) writeLine(ctx context.Context, line []byte) error {
	if f, ok := p.stdin.(*os.File); ok {
		_ = f.SetWriteDeadline(time.Now().Add(p.def.Timeout))
		defer func() { _ = f.SetWriteDeadline(time.Time{}) }()
		_, err := f.Write(line)
		return err
	}
	ch := make(chan error, 1)
	go func() { _, err := p.stdin.Write(line); ch <- err }()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(p.def.Timeout):
		return errors.New("stdio write timeout")
	}
}

// markDead 标记进程死亡并强制回收(幂等)。
func (p *stdioProcess) markDead() {
	p.dead.Store(true)
	p.kill.Do(func() {
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
		close(p.done)
	})
}

func (p *stdioProcess) touch() { p.lastUse.Store(time.Now().UnixNano()) }

func (p *stdioProcess) close() {
	if !p.dead.Load() {
		p.markDead()
	}
	_ = p.stdin.Close()
}

// spawnProcess 启动 stdio 子进程并接管 stdout。workDir 必须是 gateway 可写
// 的空目录(tmpfs); 容器网络层保证子进程无出网能力。
func spawnProcess(def Server, workDir string) (*stdioProcess, error) {
	cmd := exec.Command(def.Command, def.Args...)
	cmd.Dir = workDir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", strings.Join(append([]string{def.Command}, def.Args...), " "), err)
	}
	proc := &stdioProcess{
		def:   def,
		cmd:   cmd,
		stdin: stdin,
		lines: make(chan []byte, 64),
		done:  make(chan struct{}),
	}
	go proc.readLoop(bufio.NewReaderSize(stdout, 64*1024))
	proc.touch()
	return proc, nil
}

// readLoop 持续消费 stdout; 进程退出或 done 关闭时退出。
func (p *stdioProcess) readLoop(reader *bufio.Reader) {
	for {
		raw, err := reader.ReadBytes('\n')
		if err != nil {
			select {
			case <-p.done:
			default:
			}
			return
		}
		select {
		case p.lines <- raw:
		case <-p.done:
			return
		}
	}
}

// stdioPool 是 per-server 进程宿主: 单进程 + 崩溃退避重建 + 空闲回收。
// 不变量: acquire 返回的进程要么是已初始化(initResult 缓存非空)的存活
// 进程, 要么是刚 spawn 且尚无 initParams 的处女进程(首次 initialize 用)。
// 进程重建时自动用 initParams 重放握手(真实 MCP server 拒绝未初始化请求)。
type stdioPool struct {
	gateway     *Gateway
	def         Server
	workDir     string
	mu          sync.Mutex // 保护 proc/initResult/initParams/lastCrash
	proc        *stdioProcess
	initResult  map[string]any
	initParams  map[string]any
	initialized bool
	lastCrash   time.Time
	crashCount  int
}

// acquire 返回可用进程; 缺失/死亡且过退避窗口时重建。
// 重建时若已有 initParams, 立即向新进程重放 initialize(失败视为 spawn
// 失败: kill + 退避)。ctx 用于重建握手超时。
func (p *stdioPool) acquire(ctx context.Context) (*stdioProcess, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.proc != nil && !p.proc.dead.Load() {
		return p.proc, nil
	}
	if p.crashCount > 0 && time.Since(p.lastCrash) < DefaultCrashBackoff {
		return nil, fmt.Errorf("stdio server %s backing off after %d crash(es)", p.def.ServerID, p.crashCount)
	}
	proc, err := spawnProcess(p.def, p.workDir)
	if err != nil {
		p.lastCrash = time.Now()
		p.crashCount++
		return nil, err
	}
	if p.initParams != nil {
		if _, err := p.initializeLocked(ctx, proc, p.initParams); err != nil {
			proc.markDead()
			p.lastCrash = time.Now()
			p.crashCount++
			return nil, fmt.Errorf("reinitialize %s after rebuild: %w", p.def.ServerID, err)
		}
	}
	p.proc = proc
	p.crashCount = 0
	return proc, nil
}

// ensureInitialized 保证进程完成 MCP 握手(已初始化则返回缓存响应)。
// 必须在 acquire 成功后调用; 内部持锁, 防止并发重复握手。
func (p *stdioPool) ensureInitialized(ctx context.Context, params map[string]any) (map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.initialized && p.initResult != nil {
		return p.initResult, nil
	}
	if p.proc == nil || p.proc.dead.Load() {
		return nil, errProcessDead
	}
	resp, err := p.initializeLocked(ctx, p.proc, params)
	if err != nil {
		return nil, err
	}
	p.initParams = params
	return resp, nil
}

// initializeLocked 向进程发送 initialize(调用方必须持 pool.mu)。
func (p *stdioPool) initializeLocked(ctx context.Context, proc *stdioProcess, params map[string]any) (map[string]any, error) {
	resp, err := proc.runJSONRPC(ctx, p.gateway.nextJSONRPCID(), "initialize", params, false)
	if err != nil {
		return nil, err
	}
	p.initResult = resp
	p.initialized = true
	return resp, nil
}

func (p *stdioPool) noteCrash() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastCrash = time.Now()
	p.crashCount++
}

// reapIdle 回收 idle TTL 之外的进程; 返回是否回收。
func (p *stdioPool) reapIdle(now time.Time, idleTTL time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.proc == nil || p.proc.dead.Load() {
		return false
	}
	lastUse := time.Unix(0, p.proc.lastUse.Load())
	if now.Sub(lastUse) < idleTTL {
		return false
	}
	p.proc.close()
	p.proc = nil
	p.initialized = false
	p.initResult = nil
	return true
}

func (p *stdioPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.proc != nil {
		p.proc.close()
		p.proc = nil
	}
	p.initialized = false
	p.initResult = nil
}

func idsEqual(a, b any) bool {
	af, aok := asFloatID(a)
	bf, bok := asFloatID(b)
	if aok && bok {
		return af == bf
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func asFloatID(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
