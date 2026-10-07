// Package suggest finds the word a mistyped one was probably meant to be, so
// that a refusal can end with it.
package suggest

import (
	"fmt"
	"strings"
)

// Nearest is the candidate typed was most likely meant to be: the closest one,
// provided it is at most two steps away, or nothing. Two catches a slipped or
// swapped letter; further than that it is another word rather than a typo.
func Nearest(typed string, candidates []string) string {
	best, closest := "", 3
	for _, c := range candidates {
		if d := distance(strings.ToLower(typed), strings.ToLower(c)); d < closest {
			best, closest = c, d
		}
	}
	return best
}

// DidYouMean is the end of a sentence refusing typed: the candidate it was
// probably meant to be, as a question, or nothing where none is close.
func DidYouMean(typed string, candidates []string) string {
	if meant := Nearest(typed, candidates); meant != "" && !strings.EqualFold(meant, typed) {
		return fmt.Sprintf("; did you mean %s?", meant)
	}
	return ""
}

// distance is how many letters have to be added, removed, changed or swapped
// with their neighbour to turn a into b. A swapped pair counts once, because
// it is the commonest slip there is: zoen is one step from zone, not two.
func distance(a, b string) int {
	before, previous := []int(nil), make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		row := make([]int, len(b)+1)
		row[0] = i
		for j := 1; j <= len(b); j++ {
			change := 1
			if a[i-1] == b[j-1] {
				change = 0
			}
			row[j] = min(previous[j]+1, row[j-1]+1, previous[j-1]+change)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				row[j] = min(row[j], before[j-2]+1)
			}
		}
		before, previous = previous, row
	}
	return previous[len(b)]
}
