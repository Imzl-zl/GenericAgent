package mcpgateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// stdioProcess 是一个受管的 stdio MCP 子进程。
//
// MCP stdio 协议是单通道 request/response 串行: 所有请求经 reqMu 串行化;
// reader goroutine 持续消费 stdout, 行按序进入 lines channel; 请求方循环
// 匹配 jsonrpc id(容忍进程输出非 JSON 行)。queueLen 供进程池调度选择
// 最空闲进程。
type stdioProcess struct {
	def     Server
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lines   chan []byte
	done    chan struct{}
	reqMu   sync.Mutex
	queue   atomic.Int64 // 在途请求数(供池调度)
	lastUse atomic.Int64 // unix nano, 供空闲回收
	dead    atomic.Bool
	kill    sync.Once
}

// runJSONRPC 串行发送一条 JSON-RPC 并等待同 id 的响应。
// reqID 必须是客户端原始 id(number/string, 通知为 nil): gateway 不自造 id,
// 进程回显什么就原样返回什么。
//
// 超时(def.Timeout)kill 进程: 挂死的进程不可信——超过配置时限仍未响应的
// 进程大概率已死锁, 响应流已被污染, 必须重建。客户端 ctx 取消(断连)则
// 不 kill: 响应晚到会被后续请求按 id 匹配自然丢弃, 进程仍健康。
func (p *stdioProcess) runJSONRPC(
	ctx context.Context, reqID any, method string, params map[string]any, notification bool,
) (map[string]any, error) {
	p.reqMu.Lock()
	defer p.reqMu.Unlock()
	if p.dead.Load() {
		return nil, errProcessDead
	}
	p.queue.Add(1)
	defer p.queue.Add(-1)

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
		case <-timer.C:
			// 配置时限内未响应: 进程不可信, 必须重建。
			p.markDead()
			return nil, fmt.Errorf("stdio response timeout after %s", p.def.Timeout)
		case <-ctx.Done():
			// 客户端已离开: 响应流未被污染, 不 kill, 等待中的响应
			// 由后续请求按 id 匹配忽略。
			return nil, fmt.Errorf("await stdio response: %w", ctx.Err())
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

// spawnProcess 启动 stdio 子进程并接管 stdout。
//
// 安全边界: cmd.Env 显式设置为白名单环境(PATH/HOME/TMPDIR), 绝不继承
// gateway 自身环境——gateway 持有 DATABASE_URL 等凭据, 子进程无凭据基线
// (无网 + 无凭据, 双保险)。
func spawnProcess(def Server, workDir string) (*stdioProcess, error) {
	cmd := exec.Command(def.Command, def.Args...)
	cmd.Dir = workDir
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + workDir,
		"TMPDIR=" + workDir,
		"LANG=C.UTF-8",
	}
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

// readLoop 持续消费 stdout; 单行超过 maxResponseLineBytes 视为响应流不可信
// (无法对齐后续行), kill 进程。退出时关闭 lines(进程 stdout 已结束,
// 等待中的请求立即失败, 不必等满超时)。
func (p *stdioProcess) readLoop(reader *bufio.Reader) {
	defer close(p.lines)
	for {
		raw, err := readLineBounded(reader, maxResponseLineBytes)
		if err != nil {
			if errors.Is(err, errLineTooLong) {
				p.markDead()
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

var errLineTooLong = errors.New("stdio response line exceeds limit")

// readLineBounded 读取一行; 超限返回 errLineTooLong(不分配超长缓冲)。
func readLineBounded(reader *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		frag, isPrefix, err := reader.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, frag...)
		if len(buf) > limit {
			return nil, errLineTooLong
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

// stdioPool 是 per-server 进程宿主: 进程池(≤max_instances) + 崩溃指数退避
// + 熔断探活 + 配置 revision 热更新 + 空闲回收。
//
// 不变量:
//   - acquire 返回的进程要么已完成握手(initResult 缓存非空), 要么是刚
//     spawn 且按 initParams 重放握手成功的进程;
//   - 进程重建时自动用 initParams 重放握手(真实 MCP server 拒绝未初始化
//     请求); 首个 initialize 的 params 即进程绑定参数(shared 隔离语义)。
type stdioPool struct {
	gateway *Gateway
	def     Server
	workDir string

	mu          sync.Mutex // 保护 procs/initResult/initParams/崩溃状态
	procs       []*stdioProcess
	initResult  map[string]any
	initParams  map[string]any
	initialized bool
	crashCount  int
	lastCrash   time.Time
	nextProbe   time.Time // 熔断后的探活时刻
}

// backoffDelay 按连续失败次数指数退避: 2^count 秒, 上限 maxCrashBackoff。
func (p *stdioPool) backoffDelay() time.Duration {
	delay := DefaultCrashBackoff
	for i := 0; i < p.crashCount && delay < maxCrashBackoff; i++ {
		delay *= 2
	}
	if delay > maxCrashBackoff {
		delay = maxCrashBackoff
	}
	return delay
}

// acquire 返回一个可用的存活进程; 池空/全死时按退避与熔断状态重建。
// ctx 用于重建握手超时。
func (p *stdioPool) acquire(ctx context.Context) (*stdioProcess, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	alive := make([]*stdioProcess, 0, len(p.procs))
	for _, proc := range p.procs {
		if !proc.dead.Load() {
			alive = append(alive, proc)
		}
	}
	if len(alive) > 0 {
		// 有存活进程: 只要还有空闲进程(或已达实例上限)就直接复用;
		// 全部繁忙且有扩容余量时尝试扩容(失败回退排队)。
		if !allBusy(alive) || len(alive) >= p.def.MaxInstance {
			return pickLeastBusy(alive), nil
		}
		if proc, err := p.spawnLocked(ctx); err == nil {
			return proc, nil
		}
		return pickLeastBusy(alive), nil
	}

	// 全死或池空: 按崩溃状态决定是否允许重建。
	if p.crashCount > 0 {
		if p.crashCount >= circuitBreakThreshold {
			// 熔断: 只按探活间隔尝试, 不随请求反复重建。
			if time.Now().Before(p.nextProbe) {
				return nil, errCircuitOpen
			}
		} else if time.Since(p.lastCrash) < p.backoffDelay() {
			return nil, fmt.Errorf("%w (crash count %d)", errBackoff, p.crashCount)
		}
	}

	proc, err := p.spawnLocked(ctx)
	if err != nil {
		p.noteCrashLocked()
		return nil, err
	}
	return proc, nil
}

// pickLeastBusy 选择在途请求最少的进程(round-robin 的近似)。
func pickLeastBusy(procs []*stdioProcess) *stdioProcess {
	best := procs[0]
	for _, proc := range procs[1:] {
		if proc.queue.Load() < best.queue.Load() {
			best = proc
		}
	}
	return best
}

// allBusy 判断进程池是否全部繁忙(无空闲进程)。
func allBusy(procs []*stdioProcess) bool {
	for _, proc := range procs {
		if proc.queue.Load() == 0 {
			return false
		}
	}
	return true
}

// spawnLocked 启动新进程(存活进程数 < max_instances), 必要时重放握手。
// 调用方持锁。
func (p *stdioPool) spawnLocked(ctx context.Context) (*stdioProcess, error) {
	// 存活计数(dead 进程已被 acquire 过滤, 但列表可能残留, 顺带压缩)。
	kept := p.procs[:0]
	aliveCount := 0
	for _, proc := range p.procs {
		if proc.dead.Load() {
			continue
		}
		kept = append(kept, proc)
		aliveCount++
	}
	p.procs = kept
	if aliveCount >= p.def.MaxInstance {
		return nil, fmt.Errorf("stdio server %s at max instances (%d), all busy", p.def.ServerID, p.def.MaxInstance)
	}
	proc, err := spawnProcess(p.def, p.workDir)
	if err != nil {
		return nil, err
	}
	if p.initParams != nil {
		if _, err := p.initializeLocked(ctx, proc, p.initParams, p.gateway.nextInternalID()); err != nil {
			proc.markDead()
			return nil, fmt.Errorf("reinitialize %s after rebuild: %w", p.def.ServerID, err)
		}
	}
	p.procs = append(p.procs, proc)
	return proc, nil
}

// ensureInitialized 保证至少一个进程完成 MCP 握手(已初始化则返回缓存
// 响应)。调用方必须先 acquire 成功; 内部持锁, 防止并发重复握手。
// reqID 是当前客户端的 JSON-RPC id(首次握手时使用)。
func (p *stdioPool) ensureInitialized(ctx context.Context, params map[string]any, reqID any) (map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.initialized && p.initResult != nil {
		return p.initResult, nil
	}
	proc, err := p.liveProcessLocked()
	if err != nil {
		return nil, err
	}
	resp, err := p.initializeLocked(ctx, proc, params, reqID)
	if err != nil {
		return nil, err
	}
	p.initParams = params
	return resp, nil
}

// liveProcessLocked 返回一个存活进程; 调用方持锁。
func (p *stdioPool) liveProcessLocked() (*stdioProcess, error) {
	for _, proc := range p.procs {
		if !proc.dead.Load() {
			return proc, nil
		}
	}
	return nil, errProcessDead
}

// initializeLocked 向进程发送 initialize(调用方必须持 pool.mu)。
// reqID 是握手请求的 id: 首次握手用客户端 id; 进程重建重放用内部 id
// (响应被丢弃, 只需进程正常应答)。
func (p *stdioPool) initializeLocked(ctx context.Context, proc *stdioProcess, params map[string]any, reqID any) (map[string]any, error) {
	resp, err := proc.runJSONRPC(ctx, reqID, "initialize", params, false)
	if err != nil {
		return nil, err
	}
	p.initResult = resp
	p.initialized = true
	return resp, nil
}

// noteCrash 记录一次真实失败(spawn 失败或请求失败), 推进退避窗口并
// 在达到阈值时进入熔断。
func (p *stdioPool) noteCrash() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.noteCrashLocked()
}

// noteBackoff 记录一次退避窗口内的失败: 只推进崩溃计数(指数更快),
// 不刷新 lastCrash(窗口基准仍是最近一次真实失败), 避免请求风暴
// 永久重置退避窗口。
func (p *stdioPool) noteBackoff() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.crashCount++
	if p.crashCount >= circuitBreakThreshold {
		p.nextProbe = time.Now().Add(circuitProbeInterval)
	}
}

func (p *stdioPool) noteCrashLocked() {
	p.crashCount++
	p.lastCrash = time.Now()
	if p.crashCount >= circuitBreakThreshold {
		p.nextProbe = time.Now().Add(circuitProbeInterval)
	}
}

// noteAcquireFailure 是 acquire 失败后的计数入口: 退避窗口内失败只
// 推计数不推窗口; 熔断失败不计数(探活间隔固定, 防止请求刷新探活)。
func (p *stdioPool) noteAcquireFailure(err error) {
	switch {
	case errors.Is(err, errBackoff):
		p.noteBackoff()
	default:
		// errCircuitOpen: 不计数; spawn/reinitialize 失败已在 acquire 内计数。
	}
}

// resetCrashes 在成功服务后复位崩溃计数(熔断解除)。
func (p *stdioPool) resetCrashes() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.crashCount > 0 {
		p.crashCount = 0
	}
}

// refreshConfig 检测配置 revision 变化: 变化时排空旧进程(滚动重建)。
func (p *stdioPool) refreshConfig(def Server) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.def.Revision == def.Revision {
		return
	}
	p.def = def
	for _, proc := range p.procs {
		proc.close()
	}
	p.procs = nil
	p.initialized = false
	p.initResult = nil
	// initParams 保留: 重建时重放同一握手参数。
	p.crashCount = 0
}

// reapIdle 回收 idle TTL 之外的进程; 返回是否回收了任何进程。
func (p *stdioPool) reapIdle(now time.Time, idleTTL time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	reaped := false
	kept := p.procs[:0]
	for _, proc := range p.procs {
		if proc.dead.Load() {
			reaped = true
			continue
		}
		lastUse := time.Unix(0, proc.lastUse.Load())
		if now.Sub(lastUse) >= idleTTL {
			proc.close()
			reaped = true
			continue
		}
		kept = append(kept, proc)
	}
	p.procs = kept
	if reaped {
		// 全部回收时进程状态失效, 下次请求重建(重放握手)。
		if len(p.procs) == 0 {
			p.initialized = false
			p.initResult = nil
		}
	}
	return reaped
}

func (p *stdioPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, proc := range p.procs {
		proc.close()
	}
	p.procs = nil
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
