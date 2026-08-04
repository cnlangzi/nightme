// Package feishu — Layer 1 transient error retry
//
// 设计原则（详见 docs/channel/feishu.md §15.x / docs/feat/F-35-ratelimit.md §9）：
//   - 重试在 sendContent 外层做（包 send() 调用），限速在 SDK call 内层
//   - 两层正交：F-35 limiter 防 230001；本模块防 transient 网络/超时抖动
//   - 不重试 Feishu 业务错误码（230011/231003 terminal → 走 fallback；
//     230001 rate-limit → limiter 应已防住，触发了说明配置需调整，
//     透传给上层比沉默吞掉更好）
//   - 重试预算保守：3 次（initial + 2 retry），500ms→1s 指数退避，25% jitter
package feishu

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"syscall"
	"time"
)

// DefaultRetryConfig 是 Layer 1 重试的默认配置。
//
//	MaxAttempts    = 3     initial + 2 retries（最多 3 次 SDK call）
//	InitialBackoff = 500ms 第 1 次 retry 前的等待
//	MaxBackoff     = 5s    backoff 增长上限
//	JitterPercent  = 0.25  ±25% 抖动避免 thundering herd
var DefaultRetryConfig = RetryConfig{
	MaxAttempts:    3,
	InitialBackoff: 500 * time.Millisecond,
	MaxBackoff:     5 * time.Second,
	JitterPercent:  0.25,
}

// RetryConfig 配置 WithTransientRetry 的行为。生产代码应使用
// DefaultRetryConfig；测试可注入更短 backoff 加速。
type RetryConfig struct {
	MaxAttempts    int           // 总尝试次数（包含首次）
	InitialBackoff time.Duration // 首次 retry 前的等待
	MaxBackoff     time.Duration // 单次 backoff 上限
	JitterPercent  float64       // 抖动比例 (0.0 ~ 1.0)
}

// normalize fills in zero / negative fields with DefaultRetryConfig
// values. Silent fallback; callers don't need to validate cfg
// themselves. Returns the (possibly mutated) cfg.
func (c RetryConfig) normalize() RetryConfig {
	if c.MaxAttempts < 1 {
		c.MaxAttempts = DefaultRetryConfig.MaxAttempts
	}
	if c.InitialBackoff <= 0 {
		c.InitialBackoff = DefaultRetryConfig.InitialBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = DefaultRetryConfig.MaxBackoff
	}
	if c.JitterPercent < 0 {
		c.JitterPercent = DefaultRetryConfig.JitterPercent
	}
	if c.JitterPercent > 1 {
		c.JitterPercent = 1
	}
	return c
}

// degradationKind enumerates the failure modes that have specific
// structured-log fields. Keep values stable — post-analysis tools
// (grep, log dashboards) match on these strings.
type degradationKind string

const (
	degradationRetryExhausted    degradationKind = "retry_exhausted"
	degradationCtxCancelWait     degradationKind = "ctx_cancel_during_wait"
	degradationCtxCancelEntry    degradationKind = "ctx_cancel_at_entry"
	degradationFallbackTopLevel  degradationKind = "fallback_to_top_level"
	degradationLimiterWaitCancel degradationKind = "limiter_wait_cancelled"
)

// logDegradation emits a warn-level structured log for a degradation
// event. Schema (post-analysis stable field names):
//
//	degradation       — one of degradationKind constants
//	op                — operation label (e.g. "send", "patch_message")
//	attempts          — number of attempts made (0 if none)
//	total_wait_ms     — cumulative wait across retries
//	final_err         — final error (transient or terminal)
//	ctx_err           — ctx.Canceled / ctx.DeadlineExceeded when applicable
//	+ opts.Attrs      — call-site extras (chat_id, message_id, ...)
//
// Always emitted at warn level so degradation events survive default
// log-level filters and can be mined post-incident.
func logDegradation(logger *slog.Logger, kind degradationKind, opts RetryOpts, attempts int, totalWait time.Duration, finalErr, ctxErr error) {
	if logger == nil {
		logger = slog.Default()
	}
	attrs := []any{
		"degradation", string(kind),
		"op", opts.Op,
		"attempts", attempts,
		"total_wait_ms", totalWait.Milliseconds(),
	}
	attrs = append(attrs, opts.attrs()...)
	if finalErr != nil {
		attrs = append(attrs, "final_err", finalErr.Error())
	}
	if ctxErr != nil {
		attrs = append(attrs, "ctx_err", ctxErr.Error())
	}
	logger.Warn("feishu degradation", attrs...)
}

// IsTransient 判断 err 是否为可重试的瞬时错误。
//
// 规则（按 cc-connect 思路 + Go 网络错误模型）：
//
//	1. net.Error.Timeout() 为 true → 重试
//	2. *net.OpError 内嵌 Timeout() → 重试
//	3. io.EOF / io.ErrUnexpectedEOF → 重试
//	4. syscall.ECONNRESET / syscall.EPIPE → 重试
//	5. err.Error() 子串匹配 "connection reset" / "broken pipe" /
//	   "i/o timeout" / "TLS handshake timeout" / "connection refused" → 重试
//
// 不重试 Feishu 业务码：
//   - 230011 / 231003：message 已撤回/删除（terminal，sendContent 有 fallback）
//   - 230001：限流（F-35 limiter 应已防住，触发了让上层看见）
//   - 其他 Feishu code：永久错误（如鉴权失败、参数错误）
//
// 错误码检测兼容 "code NNNNN" 和 "code:NNNNN" 两种字符串形态；
// 优先 type-assert，无法识别时回退到 string match。
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// Feishu business codes — never retry.
	if hasFeishuCodePrefix(err.Error(), "230011") ||
		hasFeishuCodePrefix(err.Error(), "231003") ||
		hasFeishuCodePrefix(err.Error(), "230001") {
		return false
	}

	// Network / I/O transient patterns.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}

	// Substring fallback for messages the SDK wraps without
	// preserving the underlying typed error.
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"connection reset",
		"broken pipe",
		"i/o timeout",
		"tls handshake timeout",
		"connection refused",
		"no such host", // DNS 抖动（首次解析失败后立即重试通常能恢复）
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}

	return false
}

// hasFeishuCodePrefix 检测 err msg 是否包含飞书错误码前缀。
// 兼容 "code NNNNN" 和 "code:NNNNN" 两种形态。
func hasFeishuCodePrefix(msg, code string) bool {
	return strings.Contains(msg, "code "+code) || strings.Contains(msg, "code:"+code)
}

// RetryOpts wraps WithTransientRetry parameters in a struct so call
// sites can attach ad-hoc context fields (chat_id, message_id, …) for
// the degradation log schema.
type RetryOpts struct {
	// Op is the operation label ("send", "patch_message",
	// "add_reaction", ...). Goes into the degradation log as `op`.
	Op string

	// Cfg is the retry policy. Zero / negative fields fall back to
	// DefaultRetryConfig via normalize().
	Cfg RetryConfig

	// Logger is the structured logger. nil → slog.Default().
	Logger *slog.Logger

	// Attrs are extra structured fields attached to every log line
	// (including degradation events). Typical: chat_id, message_id,
	// root_id, message_kind.
	Attrs []any
}

// logger returns opts.Logger or slog.Default() if nil.
func (o RetryOpts) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

// attrs returns opts.Attrs as a copy.
func (o RetryOpts) attrs() []any {
	if len(o.Attrs) == 0 {
		return nil
	}
	out := make([]any, len(o.Attrs))
	copy(out, o.Attrs)
	return out
}

// WithTransientRetry 在 fn 返回 transient 错误时按指数退避重试。
//
// 行为：
//   - 最多尝试 opts.Cfg.MaxAttempts 次
//   - 每次 backoff = min(InitialBackoff * 2^attempt, MaxBackoff)
//   - 实际等待 = backoff * (1 + jitter*(2*r - 1))，r 均匀分布
//   - ctx.Done() 在任何等待点都立即返回 ctx.Err()（emit ctx cancel 降级日志）
//   - 第一次成功立即返回 nil
//   - 第一次非 transient 错误立即返回（不重试）
//   - 所有尝试都失败 → 返回最后一次的错误（emit retry exhausted 降级日志）
func WithTransientRetry(ctx context.Context, opts RetryOpts, fn func() error) error {
	logger := opts.logger()
	cfg := opts.Cfg.normalize()
	var lastErr error
	backoff := cfg.InitialBackoff
	totalWait := time.Duration(0)
	started := time.Now()

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if attempt == 1 {
				logDegradation(logger, degradationCtxCancelEntry,
					opts, 0, totalWait, nil, err)
				return err
			}
			return lastErr
		}
		err := fn()
		if err == nil {
			if attempt > 1 {
				logger.Debug("feishu transient retry succeeded",
					append([]any{"op", opts.Op, "attempt", attempt}, opts.attrs()...)...)
			}
			return nil
		}
		lastErr = err
		if !IsTransient(err) {
			return err
		}
		if attempt == cfg.MaxAttempts {
			logDegradation(logger, degradationRetryExhausted,
				opts, attempt, totalWait, err, nil)
			return err
		}

		wait := jitter(backoff, cfg.JitterPercent)
		logger.Debug("feishu transient retry scheduled",
			append([]any{
				"op", opts.Op,
				"attempt", attempt,
				"wait_ms", wait.Milliseconds(),
				"err", err,
			}, opts.attrs()...)...)

		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			totalWait += wait
		case <-ctx.Done():
			timer.Stop()
			totalWait += time.Since(started) - totalWait
			logDegradation(logger, degradationCtxCancelWait,
				opts, attempt, totalWait, lastErr, ctx.Err())
			// Callers care about ctx cancellation (daemon shutdown),
			// not the underlying transient error. Return ctx.Err().
			return ctx.Err()
		}

		backoff *= 2
		if backoff > cfg.MaxBackoff {
			backoff = cfg.MaxBackoff
		}
	}
	return lastErr
}

// WithTransientRetryMsg 是带 string 返回值的变体（sendContent
// 需要拿到飞书返回的 message_id）。
func WithTransientRetryMsg(ctx context.Context, opts RetryOpts, fn func() (string, error)) (string, error) {
	logger := opts.logger()
	cfg := opts.Cfg.normalize()
	var (
		lastMsgID string
		lastErr   error
	)
	backoff := cfg.InitialBackoff
	totalWait := time.Duration(0)
	started := time.Now()

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if attempt == 1 {
				logDegradation(logger, degradationCtxCancelEntry,
					opts, 0, totalWait, nil, err)
				return "", err
			}
			// Mid-loop ctx cancel: same as the error variant —
			// caller's ctx wins. Drop the partial message_id.
			return "", ctx.Err()
		}
		msgID, err := fn()
		if err == nil {
			if attempt > 1 {
				logger.Debug("feishu transient retry succeeded",
					append([]any{"op", opts.Op, "attempt", attempt, "message_id", msgID}, opts.attrs()...)...)
			}
			return msgID, nil
		}
		lastMsgID, lastErr = msgID, err
		if !IsTransient(err) {
			return msgID, err
		}
		if attempt == cfg.MaxAttempts {
			logDegradation(logger, degradationRetryExhausted,
				opts, attempt, totalWait, err, nil)
			return msgID, err
		}

		wait := jitter(backoff, cfg.JitterPercent)
		logger.Debug("feishu transient retry scheduled",
			append([]any{
				"op", opts.Op,
				"attempt", attempt,
				"wait_ms", wait.Milliseconds(),
				"err", err,
			}, opts.attrs()...)...)

		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			totalWait += wait
		case <-ctx.Done():
			timer.Stop()
			totalWait += time.Since(started) - totalWait
			logDegradation(logger, degradationCtxCancelWait,
				opts, attempt, totalWait, lastErr, ctx.Err())
			return msgID, ctx.Err()
		}

		backoff *= 2
		if backoff > cfg.MaxBackoff {
			backoff = cfg.MaxBackoff
		}
	}
	return lastMsgID, lastErr
}

// jitter 在 [backoff*(1-jp), backoff*(1+jp)] 区间随机一个等待时长。
// jp=0 → 固定 backoff；jp=0.25 → ±25%。
func jitter(backoff time.Duration, jp float64) time.Duration {
	if jp <= 0 {
		return backoff
	}
	if jp > 1 {
		jp = 1
	}
	// r ∈ [0, 1)
	r := rand.Float64()
	mult := 1 + jp*(2*r-1)
	return time.Duration(float64(backoff) * mult)
}