// Package secretscan rejects text that looks like a credential before it is
// written to task state. It is a safety net, not a guarantee
// (docs/STATE-CONTRACT.md, "Excluded data").
package secretscan

import (
	"fmt"
	"math"
	"regexp"
	"strings"
)

var patterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"Anthropic key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{16,}`)},
	{"OpenAI-style key", regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{20,}`)},
	{"GitHub token", regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}`)},
	{"GitLab token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}`)},
	{"Slack token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9\-]{10,}`)},
	{"AWS access key", regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
	{"JWT", regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`)},
	{"credential assignment", regexp.MustCompile(`(?i)(password|passwd|secret|api[_-]?key|token)\s*[:=]\s*["']?[^\s"']{8,}`)},
}

var tokenRe = regexp.MustCompile(`[A-Za-z0-9+/=_\-]{32,}`)
var hexRe = regexp.MustCompile(`^[0-9a-fA-F]+$`)

// Finding describes why text was rejected, without echoing the secret.
type Finding struct {
	Kind string
}

// Scan returns the first finding in text, or nil.
func Scan(text string) *Finding {
	for _, p := range patterns {
		if p.re.MatchString(text) {
			return &Finding{Kind: p.name}
		}
	}
	for _, tok := range tokenRe.FindAllString(text, -1) {
		// Git SHAs and other hex digests are legitimate evidence references.
		if hexRe.MatchString(tok) {
			continue
		}
		// Paths and dashed identifiers are not secrets.
		if strings.Count(tok, "/") > 1 || strings.Count(tok, "-") > 3 {
			continue
		}
		if entropy(tok) >= 4.5 && mixedClasses(tok) {
			return &Finding{Kind: "high-entropy string"}
		}
	}
	return nil
}

// Check returns an error suitable for showing to the writer.
func Check(field, text string) error {
	if f := Scan(text); f != nil {
		return fmt.Errorf("%s looks like it contains a secret (%s); store a secret reference instead (`agent-shiftboard secret ref add`)", field, f.Kind)
	}
	return nil
}

func entropy(s string) float64 {
	counts := map[rune]int{}
	for _, r := range s {
		counts[r]++
	}
	var h float64
	n := float64(len(s))
	for _, c := range counts {
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

func mixedClasses(s string) bool {
	var lower, upper, digit bool
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	return lower && upper && digit
}
