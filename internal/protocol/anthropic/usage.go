package anthropic

import "hoorific/internal/core"

type cacheCreationWire struct {
	FiveMinute *int64 `json:"ephemeral_5m_input_tokens"`
	OneHour    *int64 `json:"ephemeral_1h_input_tokens"`
}
type outputTokensDetailsWire struct {
	Thinking *int64 `json:"thinking_tokens"`
}

type usageWire struct {
	Input           *int64                   `json:"input_tokens"`
	Output          *int64                   `json:"output_tokens"`
	CachedInput     *int64                   `json:"cache_read_input_tokens,omitempty"`
	CacheWriteInput *int64                   `json:"cache_creation_input_tokens,omitempty"`
	CacheCreation   *cacheCreationWire       `json:"cache_creation,omitempty"`
	OutputDetails   *outputTokensDetailsWire `json:"output_tokens_details,omitempty"`
}

func addUsage(a, b int64) (int64, bool) {
	if b > 0 && a > (1<<63-1)-b {
		return 0, false
	}
	if b < 0 && a < (-1<<63)-b {
		return 0, false
	}
	return a + b, true
}

func usageNonnegative(n *int64) bool { return n == nil || *n >= 0 }

func usageWriteCount(u *usageWire) (*int64, error) {
	if u == nil {
		return nil, nil
	}
	if !usageNonnegative(u.CacheWriteInput) {
		return nil, unsupported("usage.cache_creation_input_tokens")
	}
	if u.CacheCreation == nil {
		return u.CacheWriteInput, nil
	}
	if !usageNonnegative(u.CacheCreation.FiveMinute) || !usageNonnegative(u.CacheCreation.OneHour) {
		return nil, unsupported("usage.cache_creation")
	}
	if u.CacheCreation.FiveMinute == nil || u.CacheCreation.OneHour == nil {
		// A missing TTL detail is unknown, not an explicit zero. Keep the
		// provider aggregate when available; otherwise the canonical total
		// cannot include an unknown cache-write subset.
		return u.CacheWriteInput, nil
	}
	v, ok := addUsage(*u.CacheCreation.FiveMinute, *u.CacheCreation.OneHour)
	if !ok {
		return nil, unsupported("usage.cache_creation")
	}
	if u.CacheWriteInput != nil {
		// The aggregate is authoritative when present. TTL details are
		// provider metadata and may be partial or inconsistent.
		return u.CacheWriteInput, nil
	}
	return &v, nil
}

func usageFromWire(u *usageWire) (*core.Usage, error) {
	if u == nil {
		return nil, nil
	}
	if !usageNonnegative(u.Input) || !usageNonnegative(u.Output) || !usageNonnegative(u.CachedInput) || (u.OutputDetails != nil && !usageNonnegative(u.OutputDetails.Thinking)) {
		return nil, unsupported("usage")
	}
	if u.CacheCreation != nil && (!usageNonnegative(u.CacheCreation.FiveMinute) || !usageNonnegative(u.CacheCreation.OneHour)) {
		return nil, unsupported("usage.cache_creation")
	}
	write, err := usageWriteCount(u)
	if err != nil {
		return nil, err
	}
	v := &core.Usage{Input: u.Input, Output: u.Output, CachedInput: u.CachedInput, CacheWriteInput: write, Source: "anthropic"}
	if u.CacheCreation != nil {
		v.CacheWrite5mInput = u.CacheCreation.FiveMinute
		v.CacheWrite1hInput = u.CacheCreation.OneHour
	}
	if u.OutputDetails != nil {
		v.ReasoningOutput = u.OutputDetails.Thinking
	}
	if v.Input != nil {
		total := *v.Input
		if v.CachedInput != nil {
			var ok bool
			total, ok = addUsage(total, *v.CachedInput)
			if !ok {
				return nil, unsupported("usage")
			}
		}
		if write != nil {
			var ok bool
			total, ok = addUsage(total, *write)
			if !ok {
				return nil, unsupported("usage")
			}
		}
		v.Input = &total
	}
	if v.Input != nil && v.Output != nil {
		total, ok := addUsage(*v.Input, *v.Output)
		if !ok {
			return nil, unsupported("usage")
		}
		v.Total = &total
	}
	return v, nil
}

func usageToWire(u *core.Usage) (*usageWire, error) {
	if u == nil {
		return nil, nil
	}
	if !usageNonnegative(u.Input) || !usageNonnegative(u.Output) || !usageNonnegative(u.CachedInput) || !usageNonnegative(u.CacheWriteInput) || !usageNonnegative(u.CacheWrite5mInput) || !usageNonnegative(u.CacheWrite1hInput) || !usageNonnegative(u.ReasoningOutput) {
		return nil, unsupported("usage")
	}
	w := &usageWire{Output: u.Output, CachedInput: u.CachedInput}
	if u.Input != nil {
		base := *u.Input
		if u.CachedInput != nil {
			if *u.CachedInput > base {
				base = 0
			} else {
				base -= *u.CachedInput
			}
		}
		write := u.CacheWriteInput
		if write == nil {
			var total int64
			ok := true
			if u.CacheWrite5mInput != nil {
				total, ok = addUsage(total, *u.CacheWrite5mInput)
			}
			if ok && u.CacheWrite1hInput != nil {
				total, ok = addUsage(total, *u.CacheWrite1hInput)
			}
			if !ok {
				return nil, unsupported("usage")
			}
			if u.CacheWrite5mInput != nil || u.CacheWrite1hInput != nil {
				write = &total
			}
		}
		if write != nil {
			if *write >= base {
				base = 0
			} else {
				base -= *write
			}
			w.CacheWriteInput = write
		}
		w.Input = &base
	} else {
		w.CacheWriteInput = u.CacheWriteInput
	}
	if u.CacheWrite5mInput != nil || u.CacheWrite1hInput != nil {
		w.CacheCreation = &cacheCreationWire{FiveMinute: u.CacheWrite5mInput, OneHour: u.CacheWrite1hInput}
	}
	if u.ReasoningOutput != nil {
		w.OutputDetails = &outputTokensDetailsWire{Thinking: u.ReasoningOutput}
	}
	return w, nil
}

func mergeUsageWire(dst *usageWire, src *usageWire) {
	if dst == nil || src == nil {
		return
	}
	if src.Input != nil {
		dst.Input = src.Input
	}
	if src.Output != nil {
		dst.Output = src.Output
	}
	if src.CachedInput != nil {
		dst.CachedInput = src.CachedInput
	}
	if src.CacheWriteInput != nil {
		dst.CacheWriteInput = src.CacheWriteInput
	}
	if src.CacheCreation != nil {
		if dst.CacheCreation == nil {
			dst.CacheCreation = &cacheCreationWire{}
		}
		if src.CacheCreation.FiveMinute != nil {
			dst.CacheCreation.FiveMinute = src.CacheCreation.FiveMinute
		}
		if src.CacheCreation.OneHour != nil {
			dst.CacheCreation.OneHour = src.CacheCreation.OneHour
		}
	}
	if src.OutputDetails != nil && src.OutputDetails.Thinking != nil {
		if dst.OutputDetails == nil {
			dst.OutputDetails = &outputTokensDetailsWire{}
		}
		dst.OutputDetails.Thinking = src.OutputDetails.Thinking
	}
}
