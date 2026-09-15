package largevalue

import (
	"fmt"
	"strconv"
	"strings"
)

type ByteRange struct {
	Start int64
	End   int64
	Total int64
}

func (r ByteRange) Length() int64 { return r.End - r.Start + 1 }

// ParseByteRange parses one HTTP bytes range. Multiple ranges are deliberately
// unsupported by the core profile and remain an adapter capability.
func ParseByteRange(value string, total int64) (ByteRange, error) {
	if total <= 0 || !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return ByteRange{}, ErrRangeUnsatisfied
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return ByteRange{}, ErrRangeUnsatisfied
	}
	var start, end int64
	var err error
	switch {
	case parts[0] == "":
		suffix, parseErr := strconv.ParseInt(parts[1], 10, 64)
		if parseErr != nil || suffix <= 0 {
			return ByteRange{}, ErrRangeUnsatisfied
		}
		if suffix > total {
			suffix = total
		}
		start, end = total-suffix, total-1
	case parts[1] == "":
		start, err = strconv.ParseInt(parts[0], 10, 64)
		end = total - 1
	default:
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err == nil {
			end, err = strconv.ParseInt(parts[1], 10, 64)
		}
	}
	if err != nil || start < 0 || start >= total || end < start {
		return ByteRange{}, ErrRangeUnsatisfied
	}
	if end >= total {
		end = total - 1
	}
	return ByteRange{Start: start, End: end, Total: total}, nil
}

func (r ByteRange) ContentRange() string {
	return fmt.Sprintf("bytes %d-%d/%d", r.Start, r.End, r.Total)
}

type ResumeRequest struct {
	Offset            int64
	ChunkLength       int64
	TotalLength       int64
	Validator         string
	ExpectedValidator string
}

// ValidateResume requires an exact offset, bounded chunk, and stable validator.
func ValidateResume(request ResumeRequest) error {
	if request.Offset < 0 || request.ChunkLength <= 0 || request.TotalLength <= 0 ||
		request.Offset > request.TotalLength-request.ChunkLength || request.Validator == "" ||
		request.Validator != request.ExpectedValidator {
		return ErrRangeUnsatisfied
	}
	return nil
}

type DownloadRequest struct {
	Method      string
	Range       string
	IfMatch     string
	IfNoneMatch string
	IfRange     string
	Length      int64
	ETag        string
}

type DownloadDecision struct {
	Status         int
	Range          *ByteRange
	ContentLength  int64
	ETag           string
	CacheControl   string
	FollowRedirect bool
}

// EvaluateDownloadRequest applies the core single-range and strong-validator
// profile. Capability responses are private and non-cacheable by default, and
// redirects are never followed without a fresh egress and authorization gate.
func EvaluateDownloadRequest(request DownloadRequest) (DownloadDecision, error) {
	decision := DownloadDecision{Status: 200, ContentLength: request.Length, ETag: request.ETag, CacheControl: "private, no-store"}
	if (request.Method != "GET" && request.Method != "HEAD") || request.Length < 0 || !strongETag(request.ETag) {
		return DownloadDecision{}, ErrRangeUnsatisfied
	}
	if request.IfMatch != "" && !etagMatches(request.IfMatch, request.ETag) {
		decision.Status = 412
		decision.ContentLength = 0
		return decision, nil
	}
	if request.IfNoneMatch != "" && etagMatches(request.IfNoneMatch, request.ETag) {
		decision.Status = 304
		decision.ContentLength = 0
		return decision, nil
	}
	if request.Range == "" || (request.IfRange != "" && request.IfRange != request.ETag) {
		return decision, nil
	}
	selected, err := ParseByteRange(request.Range, request.Length)
	if err != nil {
		decision.Status = 416
		decision.ContentLength = 0
		return decision, err
	}
	decision.Status = 206
	decision.Range = &selected
	decision.ContentLength = selected.Length()
	return decision, nil
}

func strongETag(value string) bool {
	return len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' && !strings.HasPrefix(value, "W/")
}

func etagMatches(field, current string) bool {
	for _, candidate := range strings.Split(field, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == current {
			return true
		}
	}
	return false
}
