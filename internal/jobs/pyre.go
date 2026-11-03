package jobs

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Regexp is a Python re pattern run on Go's RE2 so that it matches what
// Python's re matched on a str: \b, \w and \s are Unicode-aware there and
// ASCII-only in RE2. The text is matched through a folded copy in which every
// non-ASCII letter or number is '_' (a word character to RE2, like it is to
// Python), and \s in the pattern becomes Python's set of Unicode spaces.
// Lookaround, backreferences and $ aren't supported; none of the patterns
// use them.
type Regexp struct{ re *regexp.Regexp }

// pySpace is what \s matches in Python (str.isspace()).
const pySpace = `\t\n\v\f\r \x1c-\x1f\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

// Compile translates a Python pattern; ignoreCase is re.I.
func Compile(pattern string, ignoreCase bool) (*Regexp, error) {
	var b strings.Builder
	if ignoreCase {
		b.WriteString("(?i)")
	}
	inClass := false
	for i := 0; i < len(pattern); {
		r, size := utf8.DecodeRuneInString(pattern[i:])
		switch {
		case r == '\\' && i+1 < len(pattern):
			next := pattern[i+1]
			if next == 's' {
				if inClass {
					b.WriteString(pySpace)
				} else {
					b.WriteString("[" + pySpace + "]")
				}
			} else {
				b.WriteByte('\\')
				b.WriteByte(next)
			}
			i += 2
			continue
		case r == '[' && !inClass:
			inClass = true
		case r == ']' && inClass:
			inClass = false
		}
		b.WriteString(string(foldRune(r)))
		i += size
	}
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, err
	}
	return &Regexp{re}, nil
}

// MustCompile is Compile that panics on a bad pattern.
func MustCompile(pattern string, ignoreCase bool) *Regexp {
	re, err := Compile(pattern, ignoreCase)
	if err != nil {
		panic(err)
	}
	return re
}

// foldRune maps what Python counts as a word character outside ASCII to '_'.
// The few that Python's re.I equates with an ASCII letter become that letter.
func foldRune(r rune) rune {
	if r < utf8.RuneSelf {
		return r
	}
	switch r {
	case '\u212a': // KELVIN SIGN
		return 'k'
	case '\u017f': // LATIN SMALL LETTER LONG S
		return 's'
	case '\u0130', '\u0131': // capital I with dot, dotless i
		return 'i'
	}
	if unicode.IsLetter(r) || unicode.IsNumber(r) {
		return '_'
	}
	return r
}

// fold returns the text RE2 matches, plus for each of its bytes the offset
// of the byte it came from (nil when nothing changed).
func fold(s string) (string, []int) {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return s, nil
	}
	var b strings.Builder
	offs := make([]int, 0, len(s)+1)
	for i, r := range s {
		f := foldRune(r)
		if f == r {
			_, size := utf8.DecodeRuneInString(s[i:])
			b.WriteString(s[i : i+size])
			for k := 0; k < size; k++ {
				offs = append(offs, i+k)
			}
			continue
		}
		b.WriteRune(f)
		offs = append(offs, i)
	}
	offs = append(offs, len(s))
	return b.String(), offs
}

// MatchString is re.search(...) is not None.
func (r *Regexp) MatchString(s string) bool {
	f, _ := fold(s)
	return r.re.MatchString(f)
}

// FindAllStringSubmatch is re.finditer: for each match, the whole match and
// its groups ("" for a group that didn't take part), cut from s itself.
func (r *Regexp) FindAllStringSubmatch(s string, n int) [][]string {
	f, offs := fold(s)
	var out [][]string
	for _, idx := range r.re.FindAllStringSubmatchIndex(f, n) {
		groups := make([]string, len(idx)/2)
		for g := range groups {
			start, end := idx[2*g], idx[2*g+1]
			if start < 0 {
				continue
			}
			if offs != nil {
				start, end = offs[start], offs[end]
			}
			groups[g] = s[start:end]
		}
		out = append(out, groups)
	}
	return out
}

// FindStringSubmatch is re.search with its groups, or nil.
func (r *Regexp) FindStringSubmatch(s string) []string {
	if m := r.FindAllStringSubmatch(s, 1); len(m) > 0 {
		return m[0]
	}
	return nil
}

// FindAllString is re.findall for a pattern without groups.
func (r *Regexp) FindAllString(s string, n int) []string {
	var out []string
	for _, m := range r.FindAllStringSubmatch(s, n) {
		out = append(out, m[0])
	}
	return out
}

// isPySpace is str.isspace() for one character.
func isPySpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// Strip is str.strip().
func Strip(s string) string { return strings.TrimFunc(s, isPySpace) }

// pyFields is str.split() with no arguments.
func pyFields(s string) []string { return strings.FieldsFunc(s, isPySpace) }

// Normalize collapses runs of whitespace: " ".join(text.split()).
func Normalize(text string) string { return strings.Join(pyFields(text), " ") }
