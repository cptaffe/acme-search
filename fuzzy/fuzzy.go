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

type matcher struct {
	// Lowercased
	needle   string
	haystack string

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

func newMatcher(needle string, haystack string) *matcher {
	if len(needle) > len(haystack) {
		return nil
	}

	return &matcher{
		needle:     strings.ToLower(needle),
		haystack:   strings.ToLower(haystack),
		matchBonus: precomputeBonus(haystack),
	}
}

func (m *matcher) matchRow(i int, nr rune, curr_D []Score, curr_M []Score, last_D []Score, last_M []Score) {
	prevScore := MinScore
	var gapScore Score
	if i == len(m.needle)-1 {
		gapScore = ScoreGapTrailing
	} else {
		gapScore = ScoreGapInner
	}

	for j, r := range m.haystack {
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

func (m *matcher) Match() Score {
	/*
	 * D[][] Stores the best score for this position ending with a match.
	 * M[][] Stores the best possible score at this position.
	 */
	var D, M [2][]Score
	for i := range 2 {
		D[i] = make([]Score, len(m.haystack))
		M[i] = make([]Score, len(m.haystack))
	}
	var (
		last_D = D[0]
		last_M = M[0]
		curr_D = D[1]
		curr_M = M[1]
	)

	for i, r := range m.needle {
		m.matchRow(i, r, curr_D, curr_M, last_D, last_M)

		curr_D, last_D = last_D, curr_D
		curr_M, last_M = last_M, curr_M
	}

	return last_M[len(m.haystack)-1]
}

func (m *matcher) MatchPositions() (Score, []int) {
	/*
	 * D[][] Stores the best score for this position ending with a match.
	 * M[][] Stores the best possible score at this position.
	 */
	var (
		positions                                = make([]int, len(m.needle))
		D                              [][]Score = make([][]Score, len(m.needle))
		M                              [][]Score = make([][]Score, len(m.needle))
		last_D, last_M, curr_D, curr_M []Score
	)

	for i, _ := range m.haystack {
		D[i] = make([]Score, len(m.haystack))
		M[i] = make([]Score, len(m.haystack))
	}

	for i, r := range m.needle {
		curr_D = D[i]
		curr_M = M[i]

		m.matchRow(i, r, curr_D, curr_M, last_D, last_M)

		last_D = curr_D
		last_M = curr_M
	}

	// Backtrack to find the positions of optimal matching
	matchRequired := false
	for i, j := len(m.needle)-1, len(m.haystack)-1; i >= 0; i-- {
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

	return M[len(m.needle)-1][len(m.haystack)-1], positions
}

func Match(needle string, haystack string) Score {
	if needle == "" {
		return MinScore
	}

	if len(needle) > len(haystack) {
		/*
		 * Unreasonably large candidate: return no score
		 * If it is a valid match it will still be returned, it will
		 * just be ranked below any reasonably sized candidates
		 */
		return MinScore
	}

	return newMatcher(needle, haystack).Match()
}

func MatchPositions(needle string, haystack string) (Score, []int) {
	if len(needle) > len(haystack) {
		/*
		 * Unreasonably large candidate: return no score
		 * If it is a valid match it will still be returned, it will
		 * just be ranked below any reasonably sized candidates
		 */
		return MinScore, nil
	}

	return newMatcher(needle, haystack).MatchPositions()
}
