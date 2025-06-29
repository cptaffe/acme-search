// Translated from https://github.com/microsoft/vscode/blob/main/src/vs/base/common/fuzzyScorer.ts
package fuzzy

import (
	"strings"
)

const NoMatch = 0

func ScoreFuzzy(query, target string, allowNonContiguous bool) int {
	queryLower := strings.ToLower(query)
	targetLower := strings.ToLower(target)

	scores := make([]int, len(query)*len(target))
	matches := make([]int, len(query)*len(target))

	for qi := 0; qi < len(query); qi++ {
		offset := qi * len(target)
		prevOffset := offset - len(target)
		qChar := query[qi]
		qLower := queryLower[qi]
		for ti := 0; ti < len(target); ti++ {
			idx := offset + ti
			left := 0
			if ti > 0 {
				left = scores[idx-1]
			}
			diag := 0
			seq := 0
			if qi > 0 && ti > 0 {
				diag = scores[prevOffset+ti-1]
				seq = matches[prevOffset+ti-1]
			}
			score := computeCharScore(qChar, qLower, target, targetLower, ti, seq)
			if score > 0 && diag+score >= left && (allowNonContiguous || qi == 0 || strings.HasPrefix(targetLower[ti:], queryLower)) {
				scores[idx] = diag + score
				matches[idx] = seq + 1
			} else {
				scores[idx] = left
				matches[idx] = NoMatch
			}
		}
	}

	return scores[len(query)*len(target)-1]
}

func computeCharScore(qChar, qLower byte, target, targetLower string, ti, seq int) int {
	if !equalChar(qLower, targetLower[ti]) {
		return 0
	}
	score := 1
	if seq > 0 {
		if seq <= 3 {
			score += seq * 6
		} else {
			score += 3*seq + 3
		}
	}
	if qChar == target[ti] {
		score++
	}
	if ti == 0 {
		score += 8
	} else if sepScore := separatorBonus(target[ti-1]); sepScore > 0 {
		score += sepScore
	} else if isUpper(target[ti]) && seq == 0 {
		score += 2
	}
	return score
}

func equalChar(a, b byte) bool {
	if a == b {
		return true
	}
	return (a == '/' || a == '\\') && (b == '/' || b == '\\')
}

func separatorBonus(c byte) int {
	switch c {
	case '/', '\\':
		return 5
	case '_', '-', '.', ' ', '\'', '"', ':':
		return 4
	default:
		return 0
	}
}

func isUpper(c byte) bool {
	return 'A' <= c && c <= 'Z'
}
