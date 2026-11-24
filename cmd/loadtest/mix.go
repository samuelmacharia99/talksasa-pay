package main

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
)

// Operations the generator knows.
const (
	opUpsert = "upsert" // PUT /payers/{ref}
	opCreate = "create" // POST /payment-requests
	opGet    = "get"    // GET /payment-requests/{id} (a request created in this run)
	opList   = "list"   // GET /payment-requests (one page)
	opEvents = "events" // GET /events (follows the feed)
	opCancel = "cancel" // POST /payment-requests/{id}/cancel
	opTxns   = "txns"   // GET /transactions (one page)
	opWhoami = "whoami" // GET /whoami
)

var knownOps = []string{opUpsert, opCreate, opGet, opList, opEvents, opCancel, opTxns, opWhoami}

const defaultMix = "upsert=20,create=30,get=30,list=10,events=10"

// Mix picks operations in proportion to integer weights.
type Mix struct {
	ops   []string
	cum   []int
	total int
}

// ParseMix parses "op=weight,op=weight". Weights are non-negative integers
// and at least one must be positive.
func ParseMix(s string) (Mix, error) {
	var m Mix
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, w, ok := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		if !ok {
			return Mix{}, fmt.Errorf("mix: %q is not op=weight", part)
		}
		if !slices.Contains(knownOps, name) {
			return Mix{}, fmt.Errorf("mix: unknown op %q (known: %s)", name, strings.Join(knownOps, ", "))
		}
		if seen[name] {
			return Mix{}, fmt.Errorf("mix: %q listed twice", name)
		}
		seen[name] = true
		weight, err := strconv.Atoi(strings.TrimSpace(w))
		if err != nil || weight < 0 || weight > 1_000_000 {
			return Mix{}, fmt.Errorf("mix: weight of %q must be an integer 0..1000000", name)
		}
		if weight == 0 {
			continue
		}
		m.total += weight
		m.ops = append(m.ops, name)
		m.cum = append(m.cum, m.total)
	}
	if m.total == 0 {
		return Mix{}, fmt.Errorf("mix: no operation has a positive weight")
	}
	return m, nil
}

// Ops lists the operations with positive weight, in input order.
func (m Mix) Ops() []string { return m.ops }

// Weight returns op's weight (0 if absent).
func (m Mix) Weight(op string) int {
	for i, o := range m.ops {
		if o == op {
			if i == 0 {
				return m.cum[0]
			}
			return m.cum[i] - m.cum[i-1]
		}
	}
	return 0
}

// Pick returns an operation chosen with probability weight/total.
func (m Mix) Pick(r *rand.Rand) string {
	n := r.IntN(m.total)
	i, _ := slices.BinarySearch(m.cum, n+1)
	return m.ops[i]
}
