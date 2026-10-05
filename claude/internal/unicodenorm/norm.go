// Package unicodenorm implements Unicode NFC and NFKC normalization, the Go
// counterparts of Python's unicodedata.normalize("NFC", s) and
// unicodedata.normalize("NFKC", s).
//
// Session paths are NFC-normalized before they are sanitized into project
// directory names, matching the CLI, so that a directory reported in
// decomposed form (macOS HFS+) maps to the same project as its composed
// spelling; NFKC backs the sanitization of session tags. The standard
// library has no normalizer and this module takes no dependencies, so the
// tables in nfc_tables.go and nfkc_tables.go are generated from Python's
// unicodedata and parsed lazily on first use.
package unicodenorm

import (
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Hangul syllable composition constants (Unicode §3.12).
const (
	hangulSBase  = 0xAC00
	hangulLBase  = 0x1100
	hangulVBase  = 0x1161
	hangulTBase  = 0x11A7
	hangulLCount = 19
	hangulVCount = 21
	hangulTCount = 28
	hangulNCount = hangulVCount * hangulTCount
	hangulSCount = hangulLCount * hangulNCount
)

type nfcData struct {
	decomp  map[rune][]rune
	ccc     map[rune]uint8
	compose map[[2]rune]rune
}

var nfcTables = sync.OnceValue(func() *nfcData {
	d := &nfcData{
		decomp:  make(map[rune][]rune, 2100),
		ccc:     make(map[rune]uint8, 1000),
		compose: make(map[[2]rune]rune, 1000),
	}
	excluded := map[rune]bool{}
	for _, f := range strings.Split(nfcCompositionExclusions, ";") {
		excluded[parseTableRune(f, "NFC")] = true
	}
	parseDecompTable(nfcDecompositions, "NFC", func(r rune, seq []rune) {
		d.decomp[r] = seq
		if len(seq) == 2 && !excluded[r] {
			d.compose[[2]rune{seq[0], seq[1]}] = r
		}
	})
	for _, rec := range strings.Split(nfcCombiningClasses, ";") {
		span, class, _ := strings.Cut(rec, ":")
		k, err := strconv.Atoi(class)
		if err != nil {
			panic("claude: corrupt NFC table: " + rec)
		}
		lo, hi, ok := strings.Cut(span, "-")
		start := parseTableRune(lo, "NFC")
		end := start
		if ok {
			end = parseTableRune(hi, "NFC")
		}
		for r := start; r <= end; r++ {
			d.ccc[r] = uint8(k)
		}
	}
	return d
})

// parseTableRune parses a hex code point of a generated table, panicking
// with the table's name when it is corrupt.
func parseTableRune(s, table string) rune {
	n, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		panic("claude: corrupt " + table + " table: " + s)
	}
	return rune(n)
}

// parseDecompTable parses a generated decomposition table of
// "cp:d1[,d2...]" hex records separated by ';', calling add for each record
// in order.
func parseDecompTable(records, table string, add func(cp rune, seq []rune)) {
	for _, rec := range strings.Split(records, ";") {
		cp, list, _ := strings.Cut(rec, ":")
		var seq []rune
		for _, f := range strings.Split(list, ",") {
			seq = append(seq, parseTableRune(f, table))
		}
		add(parseTableRune(cp, table), seq)
	}
}

// NFC returns s in Unicode Normalization Form C. Bytes that are
// not valid UTF-8 are preserved as-is and act as composition barriers.
func NFC(s string) string {
	// Everything below U+0300 is already NFC and never composes with a
	// neighbour; that covers ASCII paths without touching the tables.
	fast := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0xCC { // lead bytes of U+0300 and above
			fast = false
			break
		}
	}
	if fast {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for len(s) > 0 {
		// Split into maximal valid UTF-8 runs and single invalid bytes.
		i := 0
		for i < len(s) {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size <= 1 {
				break
			}
			i += size
		}
		if i > 0 {
			b.WriteString(string(nfcRunes([]rune(s[:i]))))
			s = s[i:]
			continue
		}
		b.WriteByte(s[0])
		s = s[1:]
	}
	return b.String()
}

// nfcRunes applies canonical decomposition, canonical ordering and
// canonical composition (UAX #15) to a sequence of valid runes.
func nfcRunes(in []rune) []rune {
	t := nfcTables()
	buf := make([]rune, 0, len(in)+4)
	for _, r := range in {
		buf = t.decompose(buf, r)
	}
	// Canonical ordering: stable-sort each run of non-starters by class.
	for i := 0; i < len(buf); {
		if t.ccc[buf[i]] == 0 {
			i++
			continue
		}
		j := i
		for j < len(buf) && t.ccc[buf[j]] != 0 {
			j++
		}
		slices.SortStableFunc(buf[i:j], func(a, b rune) int {
			return int(t.ccc[a]) - int(t.ccc[b])
		})
		i = j
	}
	if len(buf) == 0 {
		return buf
	}
	// Canonical composition (reference algorithm from UAX #15).
	starterPos := 0
	starter := buf[0]
	lastClass := int(t.ccc[starter])
	if lastClass != 0 {
		lastClass = 256 // a leading non-starter can never be composed onto
	}
	out := 1
	for i := 1; i < len(buf); i++ {
		r := buf[i]
		class := int(t.ccc[r])
		if comp, ok := t.composePair(starter, r); ok && (lastClass < class || lastClass == 0) {
			buf[starterPos] = comp
			starter = comp
			continue
		}
		if class == 0 {
			starterPos = out
			starter = r
		}
		lastClass = class
		buf[out] = r
		out++
	}
	return buf[:out]
}

func (t *nfcData) decompose(dst []rune, r rune) []rune {
	if s := r - hangulSBase; s >= 0 && s < hangulSCount {
		dst = append(dst, hangulLBase+s/hangulNCount, hangulVBase+(s%hangulNCount)/hangulTCount)
		if tIdx := s % hangulTCount; tIdx != 0 {
			dst = append(dst, hangulTBase+tIdx)
		}
		return dst
	}
	seq, ok := t.decomp[r]
	if !ok {
		return append(dst, r)
	}
	for _, c := range seq {
		dst = t.decompose(dst, c)
	}
	return dst
}

func (t *nfcData) composePair(a, b rune) (rune, bool) {
	if l, v := a-hangulLBase, b-hangulVBase; l >= 0 && l < hangulLCount && v >= 0 && v < hangulVCount {
		return hangulSBase + (l*hangulVCount+v)*hangulTCount, true
	}
	if s, ti := a-hangulSBase, b-hangulTBase; s >= 0 && s < hangulSCount && s%hangulTCount == 0 && ti > 0 && ti < hangulTCount {
		return a + ti, true
	}
	r, ok := t.compose[[2]rune{a, b}]
	return r, ok
}

// nfkcCompat holds the compatibility decompositions parsed from
// nfkcCompatDecompositions.
var nfkcCompat = sync.OnceValue(func() map[rune][]rune {
	m := make(map[rune][]rune, 4000)
	parseDecompTable(nfkcCompatDecompositions, "NFKC", func(cp rune, seq []rune) { m[cp] = seq })
	return m
})

// NFKC returns s in Unicode Normalization Form KC, the Go
// counterpart of unicodedata.normalize("NFKC", s). Invalid UTF-8 becomes
// U+FFFD.
func NFKC(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return s // ASCII has no decompositions
	}
	// NFKC is canonical composition of the full compatibility
	// decomposition; nfcRunes performs the reordering and composition.
	compat, canon := nfkcCompat(), nfcTables().decomp
	buf := make([]rune, 0, len(s))
	var decompose func(r rune)
	decompose = func(r rune) {
		seq, ok := compat[r]
		if !ok {
			seq, ok = canon[r]
		}
		if !ok {
			buf = append(buf, r) // Hangul syllables are left to nfcRunes
			return
		}
		for _, c := range seq {
			decompose(c)
		}
	}
	for _, r := range s {
		decompose(r)
	}
	return string(nfcRunes(buf))
}
