// Translated from https://github.com/jhawthorn/fzy/blob/5ae3b953318becfafc1a54b994fce4ec7a8d99a8/src/match.c
package fuzzy

import (
	"math"
	"strings"
	"unicode"
)

type Score float64

var (
	MaxScore Score = Score(math.Inf(1))
	MinScore Score = Score(math.Inf(-1))
)

const (
	ScoreGapLeading       Score = -0.005
	ScoreGapTrailing      Score = -0.005
	ScoreGapInner         Score = -0.01
	ScoreMatchConsecutive Score = 1.0
	ScoreMatchSlash       Score = 0.9
	ScoreMatchWord        Score = 0.8
	ScoreMatchCapital     Score = 0.7
	ScoreMatchDot         Score = 0.6
)

type match struct {
	needleLower   string
	haystackLower string

	matchBonus []Score
}

func bonusAt(curr, prev rune) Score {
	var score Score
	if unicode.IsLetter(curr) || unicode.IsDigit(curr) {
		switch prev {
		case '/':
			return ScoreMatchSlash
		case '-':
			return ScoreMatchWord
		case '_':
			return ScoreMatchWord
		case ' ':
			return ScoreMatchWord
		case '.':
			return ScoreMatchDot
		default:
			if unicode.IsUpper(curr) && unicode.IsLower(prev) {
				return ScoreMatchCapital
			}
			return score
		}
	}
	return score
}

func precomputeBonus(haystack string) []Score {
	/* Which positions are beginning of words */
	matchBonus := make([]Score, len(haystack))
	prev := '/'
	for i, r := range haystack {
		matchBonus[i] = bonusAt(r, prev)
		prev = r
	}
	return matchBonus
}

func newMatch(needle string, haystack string) *match {
	if len(needle) > len(haystack) {
		return nil
	}

	return &match{
		needleLower:   strings.ToLower(needle),
		haystackLower: strings.ToLower(haystack),
		matchBonus:    precomputeBonus(haystack),
	}
}

func (m *match) matchRow(i int, nr rune, curr_D []Score, curr_M []Score, last_D []Score, last_M []Score) {
	prevScore := MinScore
	var gapScore Score
	if i == len(m.needleLower)-1 {
		gapScore = ScoreGapTrailing
	} else {
		gapScore = ScoreGapInner
	}

	for j, r := range m.haystackLower {
		if nr == r {
			score := MinScore
			if i == 0 {
				score = (Score(j) * ScoreGapLeading) + m.matchBonus[j]
			} else if j > 0 { // i > 0 && j > 0
				score = max(
					last_M[j-1]+m.matchBonus[j],

					// consecutive match, doesn't stack with matchBonus
					last_D[j-1]+ScoreMatchConsecutive,
				)
			}
			curr_D[j] = score
			prevScore = max(score, prevScore+gapScore)
			curr_M[j] = prevScore
		} else {
			curr_D[j] = MinScore
			prevScore += gapScore
			curr_M[j] = prevScore
		}
	}
}

func Match(needle string, haystack string) Score {
	if needle == "" {
		return MinScore
	}

	m := newMatch(needle, haystack)

	if len(needle) > len(haystack) {
		/*
		 * Unreasonably large candidate: return no score
		 * If it is a valid match it will still be returned, it will
		 * just be ranked below any reasonably sized candidates
		 */
		return MinScore
	} else if len(needle) == len(haystack) {
		/* Since this method can only be called with a haystack which
		 * matches needle. If the lengths of the strings are equal the
		 * strings themselves must also be equal (ignoring case).
		 */
		return MaxScore
	}

	/*
	 * D[][] Stores the best score for this position ending with a match.
	 * M[][] Stores the best possible score at this position.
	 */
	var D, M [2][]Score
	for i := range 2 {
		D[i] = make([]Score, len(haystack))
		M[i] = make([]Score, len(haystack))
	}
	var (
		last_D = D[0]
		last_M = M[0]
		curr_D = D[1]
		curr_M = M[1]
	)

	for i, r := range m.needleLower {
		m.matchRow(i, r, curr_D, curr_M, last_D, last_M)

		curr_D, last_D = last_D, curr_D
		curr_M, last_M = last_M, curr_M
	}

	return last_M[len(haystack)-1]
}

func MatchPositions(needle string, haystack string) (Score, []int) {
	m := newMatch(needle, haystack)
	positions := make([]int, len(needle))

	if len(needle) > len(haystack) {
		/*
		 * Unreasonably large candidate: return no score
		 * If it is a valid match it will still be returned, it will
		 * just be ranked below any reasonably sized candidates
		 */
		return MinScore, nil
	} else if len(needle) == len(haystack) {
		/* Since this method can only be called with a haystack which
		 * matches needle. If the lengths of the strings are equal the
		 * strings themselves must also be equal (ignoring case).
		 */
		for i, _ := range needle {
			positions[i] = i
		}
		return MaxScore, nil
	}

	/*
	 * D[][] Stores the best score for this position ending with a match.
	 * M[][] Stores the best possible score at this position.
	 */
	var (
		D                              [][]Score = make([][]Score, len(needle))
		M                              [][]Score = make([][]Score, len(needle))
		last_D, last_M, curr_D, curr_M []Score
	)

	for i, _ := range haystack {
		D[i] = make([]Score, len(haystack))
		M[i] = make([]Score, len(haystack))
	}

	for i, r := range m.needleLower {
		curr_D = D[i]
		curr_M = M[i]

		m.matchRow(i, r, curr_D, curr_M, last_D, last_M)

		last_D = curr_D
		last_M = curr_M
	}

	// Backtrack to find the positions of optimal matching
	matchRequired := false
	for i, j := len(needle)-1, len(haystack)-1; i >= 0; i-- {
		for ; j >= 0; j-- {
			/*
			 * There may be multiple paths which result in
			 * the optimal weight.
			 *
			 * For simplicity, we will pick the first one
			 * we encounter, the latest in the candidate
			 * string.
			 */
			if D[i][j] != MinScore &&
				(matchRequired || D[i][j] == M[i][j]) {
				/* If this score was determined using
				 * ScoreMatchConsecutive, the
				 * previous character MUST be a match
				 */
				matchRequired =
					i != 0 && j != 0 &&
						M[i][j] == D[i-1][j-1]+ScoreMatchConsecutive
				j--
				positions[i] = j
				break
			}
		}
	}

	return M[len(needle)-1][len(haystack)-1], positions
}
