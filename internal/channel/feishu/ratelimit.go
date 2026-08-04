// Package feishu — F-35 全局 token bucket 限速器
//
// 一个进程内一个 Limiter 实例（挂在 Adapter 上），覆盖所有 5 类飞书
// 出口 API（send / reply / patch / reaction / upload）。所有出口
// SDK call 前都过 Wait()，预防触发飞书 230001 / 230020 等限流错误码。
//
// 设计原则（详见 docs/feat/F-35-ratelimit.md）：
//   - 单桶覆盖所有 API（5 类文档限速完全一致：50 QPS / 1000 QPM per app）
//   - 默认 burst=1，无突发（贴飞书硬上限，不留弹性）
//   - 默认 rate_per_sec=5（per-user 硬限；nightme 热路径受此约束）
//   - Lazy refill（不启后台 goroutine，零泄漏风险）
package feishu

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/config"
)

// StrictDefault 是 F-35 的保守默认配置：
//
//	RatePerSec = 5   per-user 硬限；nightme 热路径同时满足
//	              ≤ per-group (5 QPS) / per-message_id (5 QPS) / app (50 QPS)
//	Burst      = 1   无突发；连续两次调用至少间隔 200ms
//
// 调高 = 冒触顶飞书限流错误码 230001 / 230020 风险。
var StrictDefault = config.FeishuRateLimitConfig{
	RatePerSec: 5,
	Burst:      1,
}

// clock 抽象时间来源。仅测试替换；生产用 realClock。
type clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Limiter 是一个 token bucket 限速器。
//
// 状态：tokens（当前令牌数，0..Burst） + lastRefill（上次 refill 时间）
// 所有字段在 l.mu 保护下读写；并发安全。
//
// Wait 在 tokens >= 1 时立即扣减返回；否则按 (1 - tokens) / RatePerSec
// 算出等待时长，select 等 ctx.Done() 或 timer。
type Limiter struct {
	cfg    config.FeishuRateLimitConfig
	clock  clock
	logger *slog.Logger

	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
}

// NewLimiter 构造一个 Limiter。cfg 留 nil 或字段为零值时用 StrictDefault。
// logger 留 nil 时用 slog.Default()。
func NewLimiter(cfg *config.FeishuRateLimitConfig, logger *slog.Logger) *Limiter {
	c := StrictDefault
	if cfg != nil {
		if cfg.RatePerSec > 0 {
			c.RatePerSec = cfg.RatePerSec
		}
		if cfg.Burst > 0 {
			c.Burst = cfg.Burst
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Limiter{
		cfg:        c,
		clock:      realClock{},
		logger:     logger,
		tokens:     float64(c.Burst), // 启动后立即可用 Burst 个 token
		lastRefill: realClock{}.Now(), // 生产用 realClock；测试 SetClock 后会重置
	}
}

// Wait blocks until a token is available, or ctx is cancelled.
// Returns ctx.Err() on cancel; nil on success.
//
// 行为：
//   - 桶满 → 立即扣减返回
//   - 桶空 → 按 deficit / RatePerSec 等待下一个 token
//   - ctx 取消 → 立即返回 ctx.Err()（emit 降级日志便于事后分析）
//
// 阻塞时长 > 100ms 时记 debug log（不污染 hot path 日志）。
func (l *Limiter) Wait(ctx context.Context) error {
	started := time.Now()
	for {
		l.mu.Lock()
		now := l.clock.Now()
		elapsed := now.Sub(l.lastRefill).Seconds()
		l.tokens = math.Min(float64(l.cfg.Burst), l.tokens+elapsed*l.cfg.RatePerSec)
		l.lastRefill = now

		if l.tokens >= 1.0 {
			l.tokens -= 1.0
			tokensSnapshot := l.tokens // copy for lock-free log read
			l.mu.Unlock()
			if d := time.Since(started); d > 100*time.Millisecond {
				l.logger.Debug("feishu rate limit blocked",
					"wait_ms", d.Milliseconds(),
					"tokens", tokensSnapshot,
					"rate_per_sec", l.cfg.RatePerSec,
					"burst", l.cfg.Burst,
				)
			}
			return nil
		}

		// tokens < 1 → 需要等多久才有下一个
		deficit := 1.0 - l.tokens
		waitSec := deficit / l.cfg.RatePerSec
		l.mu.Unlock()

		timer := time.NewTimer(time.Duration(waitSec * float64(time.Second)))
		select {
		case <-timer.C:
			// loop again; next iteration will refill + consume
		case <-ctx.Done():
			timer.Stop()
			// Emit degradation log so post-analysis can detect
			// daemon shutdown blocking on rate-limit waits.
			l.logger.Warn("feishu degradation",
				"degradation", string(degradationLimiterWaitCancel),
				"wait_ms", time.Since(started).Milliseconds(),
				"rate_per_sec", l.cfg.RatePerSec,
				"burst", l.cfg.Burst,
				"ctx_err", ctx.Err().Error(),
			)
			return ctx.Err()
		}
	}
}

// SetClock 替换 clock 并重置 lastRefill。仅测试使用；生产代码不要调用。
//
// 必须同时重置 lastRefill，否则新 clock 的初始时间减去旧 lastRefill
// 会得出巨大负数 elapsed，让 tokens 计算溢出，Wait 永远阻塞。
func (l *Limiter) SetClock(c clock) {
	l.mu.Lock()
	l.clock = c
	l.lastRefill = c.Now()
	l.mu.Unlock()
}

// Cfg 返回当前生效的限速配置（用于诊断日志 / status 命令）。
func (l *Limiter) Cfg() config.FeishuRateLimitConfig {
	return l.cfg
}