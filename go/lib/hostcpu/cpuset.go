// Package hostcpu provides Linux CPU topology and affinity operations.
package hostcpu

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// CPUSet is an immutable set of logical CPU identifiers.
type CPUSet struct {
	words []uint64
}

// ParseCPUSet parses the Linux cpulist format, such as "0-3,8,10-11".
func ParseCPUSet(text string) (CPUSet, error) {
	var builder cpuSetBuilder
	text = strings.TrimSpace(text)
	if text == "" {
		return CPUSet{}, nil
	}
	for _, field := range strings.Split(text, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			return CPUSet{}, fmt.Errorf("parse CPU set %q: empty field", text)
		}
		first, last, found := strings.Cut(field, "-")
		start, err := parseCPU(first)
		if err != nil {
			return CPUSet{}, fmt.Errorf("parse CPU set %q: %w", text, err)
		}
		end := start
		if found {
			if strings.Contains(last, "-") {
				return CPUSet{}, fmt.Errorf("parse CPU set %q: invalid range %q", text, field)
			}
			end, err = parseCPU(last)
			if err != nil {
				return CPUSet{}, fmt.Errorf("parse CPU set %q: %w", text, err)
			}
			if end < start {
				return CPUSet{}, fmt.Errorf("parse CPU set %q: descending range %q", text, field)
			}
		}
		builder.addRange(start, end)
	}
	return builder.freeze(), nil
}

// NewCPUSet creates a set from logical CPU identifiers.
func NewCPUSet(cpus ...int) (CPUSet, error) {
	var builder cpuSetBuilder
	for _, cpu := range cpus {
		if cpu < 0 {
			return CPUSet{}, fmt.Errorf("CPU identifier must be nonnegative: %d", cpu)
		}
		builder.add(cpu)
	}
	return builder.freeze(), nil
}

func parseCPU(text string) (int, error) {
	cpu, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil || cpu < 0 {
		return 0, fmt.Errorf("invalid CPU identifier %q", text)
	}
	return cpu, nil
}

type cpuSetBuilder struct {
	words []uint64
}

func (b *cpuSetBuilder) add(cpu int) {
	b.grow(cpu / 64)
	b.words[cpu/64] |= uint64(1) << uint(cpu%64)
}

func (b *cpuSetBuilder) addRange(start, end int) {
	firstWord, lastWord := start/64, end/64
	b.grow(lastWord)
	firstBit, lastBit := uint(start%64), uint(end%64)
	if firstWord == lastWord {
		mask := ^uint64(0) << firstBit
		mask &= ^uint64(0) >> (63 - lastBit)
		b.words[firstWord] |= mask
		return
	}
	b.words[firstWord] |= ^uint64(0) << firstBit
	for word := firstWord + 1; word < lastWord; word++ {
		b.words[word] = ^uint64(0)
	}
	b.words[lastWord] |= ^uint64(0) >> (63 - lastBit)
}

func (b *cpuSetBuilder) grow(word int) {
	if word < len(b.words) {
		return
	}
	b.words = append(b.words, make([]uint64, word+1-len(b.words))...)
}

func (b *cpuSetBuilder) freeze() CPUSet {
	set := CPUSet{words: b.words}
	b.words = nil
	return set
}

func (s CPUSet) with(cpu int) CPUSet {
	word := cpu / 64
	if word >= len(s.words) {
		words := make([]uint64, word+1)
		copy(words, s.words)
		s.words = words
	} else {
		s.words = append([]uint64(nil), s.words...)
	}
	s.words[word] |= uint64(1) << uint(cpu%64)
	return s
}

// Contains reports whether the set contains cpu.
func (s CPUSet) Contains(cpu int) bool {
	return cpu >= 0 && cpu/64 < len(s.words) && s.words[cpu/64]&(uint64(1)<<uint(cpu%64)) != 0
}

// Empty reports whether the set contains no CPUs.
func (s CPUSet) Empty() bool { return s.Count() == 0 }

// Count returns the number of CPUs in the set.
func (s CPUSet) Count() int {
	count := 0
	for _, word := range s.words {
		for word != 0 {
			word &= word - 1
			count++
		}
	}
	return count
}

// CPUs returns the CPU identifiers in ascending order.
func (s CPUSet) CPUs() []int {
	result := make([]int, 0, s.Count())
	for word, bits := range s.words {
		for bit := 0; bits != 0; bit++ {
			if bits&1 != 0 {
				result = append(result, word*64+bit)
			}
			bits >>= 1
		}
	}
	return result
}

// Union returns CPUs present in either set.
func (s CPUSet) Union(other CPUSet) CPUSet {
	return combine(s, other, func(a, b uint64) uint64 { return a | b })
}

// Intersection returns CPUs present in both sets.
func (s CPUSet) Intersection(other CPUSet) CPUSet {
	return combine(s, other, func(a, b uint64) uint64 { return a & b })
}

// Difference returns CPUs present in s but absent from other.
func (s CPUSet) Difference(other CPUSet) CPUSet {
	return combine(s, other, func(a, b uint64) uint64 { return a &^ b })
}

// IsSubsetOf reports whether every CPU in s is present in other.
func (s CPUSet) IsSubsetOf(other CPUSet) bool { return s.Difference(other).Empty() }

// Equal reports whether both sets contain the same CPUs.
func (s CPUSet) Equal(other CPUSet) bool { return s.IsSubsetOf(other) && other.IsSubsetOf(s) }

func combine(a, b CPUSet, op func(uint64, uint64) uint64) CPUSet {
	n := len(a.words)
	if len(b.words) > n {
		n = len(b.words)
	}
	words := make([]uint64, n)
	for i := range words {
		var aw, bw uint64
		if i < len(a.words) {
			aw = a.words[i]
		}
		if i < len(b.words) {
			bw = b.words[i]
		}
		words[i] = op(aw, bw)
	}
	for len(words) > 0 && words[len(words)-1] == 0 {
		words = words[:len(words)-1]
	}
	return CPUSet{words: words}
}

// String returns the canonical Linux cpulist representation.
func (s CPUSet) String() string {
	cpus := s.CPUs()
	if len(cpus) == 0 {
		return ""
	}
	sort.Ints(cpus)
	parts := make([]string, 0, len(cpus))
	for i := 0; i < len(cpus); {
		j := i
		for j+1 < len(cpus) && cpus[j+1] == cpus[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(cpus[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", cpus[i], cpus[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}
