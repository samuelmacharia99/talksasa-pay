package main

import (
	"math"
	"math/bits"
	"time"
)

// Histogram is a log-linear (HDR-style) latency histogram: values below 64 ns
// are exact; above that each power of two is split into 64 equal buckets,
// so any reported quantile is within ~1.6% of the true value. Not safe for
// concurrent use; merge per-worker histograms instead.
type Histogram struct {
	counts   []uint64
	n        uint64
	min, max uint64
	sum      float64
}

const (
	subBits    = 6
	subCount   = 1 << subBits
	numBuckets = (64 - subBits + 1) * subCount
)

// NewHistogram returns an empty histogram.
func NewHistogram() *Histogram {
	return &Histogram{counts: make([]uint64, numBuckets), min: math.MaxUint64}
}

func bucketIndex(v uint64) int {
	if v < subCount {
		return int(v)
	}
	shift := bits.Len64(v) - subBits - 1
	return (shift+1)*subCount + int(v>>uint(shift)) - subCount
}

// bucketUpper is the largest value that maps to bucket idx.
func bucketUpper(idx int) uint64 {
	if idx < subCount {
		return uint64(idx)
	}
	shift := uint(idx/subCount - 1)
	m := uint64(idx%subCount + subCount)
	return m<<shift + (1<<shift - 1)
}

// Record adds one observation (negative durations count as 0).
func (h *Histogram) Record(d time.Duration) {
	v := uint64(max(d, 0))
	h.counts[bucketIndex(v)]++
	h.n++
	h.sum += float64(v)
	h.min = min(h.min, v)
	h.max = max(h.max, v)
}

// Merge adds o's observations to h.
func (h *Histogram) Merge(o *Histogram) {
	if o.n == 0 {
		return
	}
	for i, c := range o.counts {
		h.counts[i] += c
	}
	h.n += o.n
	h.sum += o.sum
	h.min = min(h.min, o.min)
	h.max = max(h.max, o.max)
}

// Count is the number of observations.
func (h *Histogram) Count() uint64 { return h.n }

// Max is the largest observation (exact).
func (h *Histogram) Max() time.Duration {
	if h.n == 0 {
		return 0
	}
	return time.Duration(h.max)
}

// Mean is the arithmetic mean (exact).
func (h *Histogram) Mean() time.Duration {
	if h.n == 0 {
		return 0
	}
	return time.Duration(h.sum / float64(h.n))
}

// Quantile returns the value at quantile q in [0,1]: the upper bound of the
// bucket holding the ceil(q*n)-th observation, clamped to [min, max].
func (h *Histogram) Quantile(q float64) time.Duration {
	if h.n == 0 {
		return 0
	}
	rank := uint64(math.Ceil(q * float64(h.n)))
	rank = max(rank, 1)
	var cum uint64
	for i, c := range h.counts {
		cum += c
		if cum >= rank {
			u := bucketUpper(i)
			u = min(max(u, h.min), h.max)
			return time.Duration(u)
		}
	}
	return time.Duration(h.max)
}
