package toolsearch

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	weightName     = 3 // skill name
	weightKeywords = 3
	weightToolName = 2
	weightDesc     = 1 // skill and tool description
	prefixMinRunes = 4
)

var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields("a an and are as at be by cli for from how i in is it me my of on or please relay skill the to tool tools use via what with") {
		m[w] = true
	}
	return m
}()

// tokenize lower-cases s, splits on anything that is not a letter or digit
// and drops one-character tokens.
func tokenize(s string) []string {
	parts := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := parts[:0]
	for _, p := range parts {
		if utf8.RuneCountInString(p) > 1 {
			out = append(out, p)
		}
	}
	return out
}

func queryTokens(q string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tokenize(q) {
		if stopwords[t] || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func tokensMatch(q, f string) bool {
	if q == f {
		return true
	}
	if utf8.RuneCountInString(q) >= prefixMinRunes && utf8.RuneCountInString(f) >= prefixMinRunes {
		return strings.HasPrefix(q, f) || strings.HasPrefix(f, q)
	}
	return false
}

// field is one weighted bag of tokens a query token can match.
type field struct {
	weight int
	tokens []string
}

func anyMatch(q string, tokens []string) bool {
	for _, t := range tokens {
		if tokensMatch(q, t) {
			return true
		}
	}
	return false
}

// score sums, over distinct query tokens, the best weight any field matched.
func score(query []string, fields []field) int {
	total := 0
	for _, q := range query {
		best := 0
		for _, f := range fields {
			if f.weight > best && anyMatch(q, f.tokens) {
				best = f.weight
			}
		}
		total += best
	}
	return total
}
