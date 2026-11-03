package officer

import "sort"

// SequenceRatio is difflib.SequenceMatcher(None, a, b).ratio(): twice the
// number of matched characters over the total length, with the matching
// blocks found the way difflib finds them (including its autojunk rule for
// strings of 200 characters or more).
func SequenceRatio(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	total := len(ra) + len(rb)
	if total == 0 {
		return 1.0
	}
	m := newMatcher(ra, rb)
	matches := 0
	for _, blk := range m.matchingBlocks() {
		matches += blk[2]
	}
	return 2.0 * float64(matches) / float64(total)
}

type matcher struct {
	a, b []rune
	b2j  map[rune][]int
}

func newMatcher(a, b []rune) *matcher {
	b2j := map[rune][]int{}
	for i, elt := range b {
		b2j[elt] = append(b2j[elt], i)
	}
	if n := len(b); n >= 200 {
		ntest := n/100 + 1
		for elt, idxs := range b2j {
			if len(idxs) > ntest {
				delete(b2j, elt)
			}
		}
	}
	return &matcher{a, b, b2j}
}

// findLongestMatch is SequenceMatcher.find_longest_match with no junk
// function (popular characters are left out of b2j but may still extend a match).
func (m *matcher) findLongestMatch(alo, ahi, blo, bhi int) (int, int, int) {
	a, b := m.a, m.b
	besti, bestj, bestsize := alo, blo, 0
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		newj2len := map[int]int{}
		for _, j := range m.b2j[a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			newj2len[j] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		j2len = newj2len
	}
	for besti > alo && bestj > blo && a[besti-1] == b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi && a[besti+bestsize] == b[bestj+bestsize] {
		bestsize++
	}
	return besti, bestj, bestsize
}

func (m *matcher) matchingBlocks() [][3]int {
	queue := [][4]int{{0, len(m.a), 0, len(m.b)}}
	var blocks [][3]int
	for len(queue) > 0 {
		q := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		alo, ahi, blo, bhi := q[0], q[1], q[2], q[3]
		i, j, k := m.findLongestMatch(alo, ahi, blo, bhi)
		if k > 0 {
			blocks = append(blocks, [3]int{i, j, k})
			if alo < i && blo < j {
				queue = append(queue, [4]int{alo, i, blo, j})
			}
			if i+k < ahi && j+k < bhi {
				queue = append(queue, [4]int{i + k, ahi, j + k, bhi})
			}
		}
	}
	sort.Slice(blocks, func(x, y int) bool {
		if blocks[x][0] != blocks[y][0] {
			return blocks[x][0] < blocks[y][0]
		}
		return blocks[x][1] < blocks[y][1]
	})
	return blocks
}
