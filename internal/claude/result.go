package claude

import (
	"bufio"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"
)

const (
	maxStreamLine  = 16 << 20
	creditsErrCode = "credits_required"
	statusRejected = "rejected"
)

var (
	limitMessage = regexp.MustCompile(`(?i)\Ayou['’]ve hit your (session|weekly|opus|sonnet) limit\s*·\s*resets (.+)\z`)
	fableMessage = regexp.MustCompile(`(?i)\Afable limit reached\b`)
)

// ResultLine is the stream's final result object.
type ResultLine struct {
	IsError        bool    `json:"is_error"`
	APIErrorStatus int     `json:"api_error_status"`
	ErrorCode      string  `json:"errorCode"`
	NumTurns       int     `json:"num_turns"`
	DurationMS     int64   `json:"duration_ms"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	Result         string  `json:"result"`
}

// RateLimitInfo is the only structured limit signal Claude Code emits, and it
// appears in the stream form alone.
type RateLimitInfo struct {
	Status   string `json:"status"`
	ResetsAt int64  `json:"resetsAt"`
}

type streamLine struct {
	Type          string         `json:"type"`
	RateLimitInfo *RateLimitInfo `json:"rate_limit_info"`
}

// Stream is what a run said: its final result object and the last rate-limit
// event it emitted.
type Stream struct {
	result    *ResultLine
	rateLimit *RateLimitInfo
}

// Result is the run's final result object, and whether the stream carried
// one at all.
func (s Stream) Result() (ResultLine, bool) {
	if s.result == nil {
		return ResultLine{}, false
	}
	return *s.result, true
}

// ParseStream reads the newline-delimited stream, keeping the last
// rate_limit_event and the final result object. A line that is not JSON is
// not the wrapper's business.
func ParseStream(r io.Reader) (Stream, error) {
	var stream Stream
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxStreamLine)
	for scanner.Scan() {
		line := scanner.Bytes()
		var head streamLine
		if err := json.Unmarshal(line, &head); err != nil {
			continue
		}
		switch head.Type {
		case "rate_limit_event":
			if head.RateLimitInfo != nil {
				stream.rateLimit = head.RateLimitInfo
			}
		case "result":
			var result ResultLine
			if err := json.Unmarshal(line, &result); err != nil {
				continue
			}
			stream.result = &result
		}
	}
	if err := scanner.Err(); err != nil {
		return stream, err
	}
	return stream, nil
}

// Limit reports whether the run hit a limit, and when the window ends. The
// matcher is ordered because the -p limit shape is undocumented: the
// structured event first, then the 429, then the message.
func (s Stream) Limit(now time.Time) (Limit, bool) {
	result, _ := s.Result()
	kind, resetText := s.limitKind(result, s.failed(result))
	hit := (s.rateLimit != nil && s.rateLimit.Status == statusRejected) ||
		(result.IsError && result.APIErrorStatus == 429) ||
		kind != ""
	if !hit {
		return Limit{}, false
	}
	if kind == "" {
		kind = KindUnknown
	}
	return Limit{Kind: kind, ResetAt: s.resetAt(resetText, now)}, true
}

func (s Stream) failed(result ResultLine) bool {
	return result.IsError || result.APIErrorStatus == 429 || s.rateLimit != nil
}

// limitKind reads the kind out of the result object. The result text is
// model-authored, and an agent that quotes a limit message back would
// otherwise halt the whole farm, so a message counts only as the result's
// closing line and only alongside a failure signal — except the Fable notice,
// which Claude Code emits on a successful run and which is therefore taken on
// its own only when it is the entire result.
func (s Stream) limitKind(result ResultLine, failed bool) (kind, resetText string) {
	text := strings.TrimSpace(result.Result)
	last := text
	if cut := strings.LastIndexByte(text, '\n'); cut >= 0 {
		last = strings.TrimSpace(text[cut+1:])
	}
	if match := limitMessage.FindStringSubmatch(last); match != nil && failed {
		return strings.ToLower(match[1]), strings.TrimSpace(match[2])
	}
	if fableMessage.MatchString(last) && (failed || last == text) {
		return KindFable, ""
	}
	if result.ErrorCode == creditsErrCode {
		return KindCredits, ""
	}
	return "", ""
}

func (s Stream) resetAt(resetText string, now time.Time) time.Time {
	if s.rateLimit != nil && s.rateLimit.ResetsAt > 0 {
		return time.Unix(s.rateLimit.ResetsAt, 0).In(now.Location())
	}
	if reset, ok := ParseResetText(resetText, now); ok {
		return reset
	}
	return now.Add(FallbackReset)
}
