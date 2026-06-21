package cluster

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ParseSelector разбирает выражение выбора шардов и возвращает выбранное
// подмножество и нормализованную метку для prompt.
//
// Поддерживаемые формы:
//
//	""              -> все шарды           (метка "all")
//	"all"           -> все шарды           (метка "all")
//	"rs005"         -> шард с такой меткой  (метка "rs005")
//	"0,1,3..7,10"   -> позиции с нуля, через запятую, с диапазонами N..M
func ParseSelector(shards []Shard, sel string) ([]Shard, string, error) {
	sel = strings.TrimSpace(sel)
	if sel == "" || strings.EqualFold(sel, "all") {
		return shards, "all", nil
	}

	// Одиночный токен, точно совпадающий с меткой шарда, выбирает этот шард.
	// Метки имеют приоритет над позиционной интерпретацией, иначе чисто
	// числовая метка (например "001") была бы прочитана как индекс с нуля.
	// Списки и диапазоны ("0,1,3..7") всегда позиционные.
	if !strings.ContainsAny(sel, ",") && !strings.Contains(sel, "..") {
		for _, s := range shards {
			if strings.EqualFold(s.Label, sel) {
				return []Shard{s}, s.Label, nil
			}
		}
	}

	positions, err := parsePositions(sel, len(shards))
	if err != nil {
		return nil, "", err
	}
	out := make([]Shard, 0, len(positions))
	for _, p := range positions {
		out = append(out, shards[p])
	}
	return out, normalizeLabel(positions), nil
}

// parsePositions разбирает "0,1,3..7,10" в отсортированные уникальные позиции с нуля.
func parsePositions(sel string, n int) ([]int, error) {
	seen := map[int]bool{}
	var out []int
	add := func(p int) error {
		if p < 0 || p >= n {
			return fmt.Errorf("shard index %d out of range [0..%d]", p, n-1)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
		return nil
	}

	for _, tok := range strings.Split(sel, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if strings.Contains(tok, "..") {
			parts := strings.SplitN(tok, "..", 2)
			start, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
			end, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("invalid range %q (expected N..M)", tok)
			}
			if start > end {
				return nil, fmt.Errorf("invalid range %q (start > end)", tok)
			}
			for i := start; i <= end; i++ {
				if err := add(i); err != nil {
					return nil, err
				}
			}
			continue
		}
		p, err := strconv.Atoi(tok)
		if err != nil {
			return nil, fmt.Errorf("invalid shard index %q", tok)
		}
		if err := add(p); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty shard selection")
	}
	sort.Ints(out)
	return out, nil
}

// normalizeLabel сжимает позиции в компактную метку диапазонов вида "0-2,5,7-9" для prompt.
func normalizeLabel(positions []int) string {
	if len(positions) == 0 {
		return ""
	}
	var parts []string
	start := positions[0]
	prev := positions[0]
	flush := func(a, b int) {
		if a == b {
			parts = append(parts, strconv.Itoa(a))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", a, b))
		}
	}
	for _, p := range positions[1:] {
		if p == prev+1 {
			prev = p
			continue
		}
		flush(start, prev)
		start, prev = p, p
	}
	flush(start, prev)
	return strings.Join(parts, ",")
}
