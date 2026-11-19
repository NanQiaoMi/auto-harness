package agent

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Retry defaults. They mirror pi's policy: a few attempts, exponential
// backoff, and a cap on the delay so a run does not sleep for minutes.
const (
	// DefaultMaxRetries is how many times a failed request is retried.
	// The first attempt does not count.
	DefaultMaxRetries = 3
	// DefaultRetryBaseDelay is the first backoff delay.
	DefaultRetryBaseDelay = time.Second
	// DefaultRetryMaxDelay caps one backoff delay.
	DefaultRetryMaxDelay = 60 * time.Second
)

// nonRetryablePattern matches errors that will fail the same way every
// time. Retrying them only wastes the user's time and money.
//
// The list is pi's, trimmed to what the harness's providers can
// actually produce: quota and billing exhaustion, authentication and
// authorization failures, malformed requests, and the model-not-found
// family.
var nonRetryablePattern = regexp.MustCompile(`(?i)` + strings.Join([]string{
	// Account and subscription limits. These reset in hours or on a
	// billing change, not in seconds.
	"GoUsageLimitError",
	"FreeUsageLimitError",
	"usage limit reached",
	"available balance",
	"insufficient_quota",
	"out of budget",
	"quota exceeded",
	"billing",
	"subscription_sharing_usage",
	// Credentials and permissions.
	"invalid.?api.?key",
	"incorrect api key",
	"unauthorized",
	"authentication",
	"permission.?denied",
	"forbidden",
	"no such model",
	"model.?not.?found",
	"does not exist",
	"context length", // overflow is handled by compaction, not retry
	// Malformed requests.
	"invalid.?request",
	"bad.?request",
}, "|"))

// retryablePattern matches failures that are worth another attempt:
// throttling, server-side trouble, and transport failures.
var retryablePattern = regexp.MustCompile(`(?i)` + strings.Join([]string{
	"overloaded",
	"server_busy",
	"servers are currently busy",
	"currently experiencing high demand",
	"model is at capacity",
	"rate.?limit",
	"too many requests",
	`\b(?:429|500|502|503|504|520|524)\b`,
	"service.?(?:is.?)?(?:temporarily.?)?unavailable",
	"server.?error",
	"internal.?error",
	"provider.?returned.?(?:an.?)?error",
	"stream was interrupted",
	"unable to forward upstream",
	"upstream.?request.?failed",
	"network.?error",
	"connection.?(?:error|refused|lost|reset|closed)",
	"other side closed",
	"broken pipe",
	"fetch failed",
	"getaddrinfo",
	`\b(?:ECONNRESET|ECONNABORTED|ECONNREFUSED|ETIMEDOUT|EPIPE)\b`,
	"connection reset by peer",
	"ENOTFOUND",
	"EAI_AGAIN",
	"socket hang up",
	"timed? out",
	"timeout",
	"terminated",
	"ended without",
	"stream ended before",
	"unexpected EOF",
	// A bare EOF is the connection closing before a response arrived
	// (net/http reports it as `Post "…": EOF`). The normal end of a
	// stream never reaches the classifier: streamOnce treats io.EOF as
	// success.
	`\bEOF\b`,
	"http2",
	"try your request again",
	"please retry",
}, "|"))

// isRetryable reports whether a failed request is worth retrying.
//
// The non-retryable list wins: a message that looks like a transport
// failure but says "insufficient_quota" is a quota error, not a
// flaky connection.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	// A cancelled or expired context is the caller's decision, not a
	// transient failure.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := err.Error()
	if nonRetryablePattern.MatchString(msg) {
		return false
	}
	return retryablePattern.MatchString(msg)
}

// retryDelay returns the backoff before the given attempt (1-based).
//
// It doubles each time and is capped, so a long outage settles into a
// steady retry rhythm instead of sleeping for minutes.
func retryDelay(base, max time.Duration, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := base
	for i := 1; i < attempt; i++ {
		if delay >= max {
			return max
		}
		delay *= 2
	}
	if delay > max {
		return max
	}
	return delay
}

// sleepCtx waits for d, returning false when the context ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
