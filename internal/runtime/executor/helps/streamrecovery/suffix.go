package streamrecovery

// ContinuationSuffix computes the suffix needed to continue text where the
// previous stream stopped, without repeating already-emitted content. Mirrors
// continuation_suffix in recovery.py.
//
// Returns:
//   - "" when candidate is empty
//   - candidate when no existing content (whole candidate is the suffix)
//   - candidate[len(existing):] when candidate starts with existing
//   - the non-overlapping tail when existing ends with a prefix of candidate
//   - candidate when it is short enough to be a safe continuation
//   - nil when the candidate diverges significantly from existing
func ContinuationSuffix(existing, candidate string) string {
	if candidate == "" {
		return ""
	}
	if existing == "" {
		return candidate
	}
	if len(candidate) >= len(existing) && candidate[:len(existing)] == existing {
		return candidate[len(existing):]
	}
	maxOverlap := len(existing)
	if len(candidate) < maxOverlap {
		maxOverlap = len(candidate)
	}
	for size := maxOverlap; size > 0; size-- {
		if existing[len(existing)-size:] == candidate[:size] {
			return candidate[size:]
		}
	}
	if len(candidate) < maxOverlap {
		// Candidate is fully distinct from existing but short; treat as suffix.
		return candidate
	}
	threshold := 200
	if len(existing)/2 > threshold {
		threshold = len(existing) / 2
	}
	if len(candidate) < threshold {
		return candidate
	}
	return ""
}
