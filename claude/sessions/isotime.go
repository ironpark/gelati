package sessions

import (
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// ISO-8601 timestamps
// ---------------------------------------------------------------------------

// isoToEpochMillis parses an ISO-8601 timestamp to Unix epoch milliseconds
// the way the Python SDK does (datetime.fromisoformat, then
// int(timestamp() * 1000)). It accepts extended and basic dates
// (YYYY-MM-DD, YYYYMMDD), any single separator character, times of the form
// HH[:MM[:SS[.f+]]] or HHMM[SS[.f+]] ("," also accepted as the decimal mark,
// digits beyond microseconds ignored), and offsets Z, ±HH, ±HHMM and
// ±HH:MM[:SS[.ffffff]]. Timestamps without an offset are local time, as in
// Python. Week dates are not supported.
func isoToEpochMillis(ts string) (int64, bool) {
	if strings.HasSuffix(ts, "Z") {
		ts = strings.ReplaceAll(ts, "Z", "+00:00")
	}
	year, month, day, rest, ok := parseISODate(ts)
	if !ok {
		return 0, false
	}
	var hour, minute, sec, micro, offset int
	aware := false
	if rest != "" {
		_, size := utf8.DecodeRuneInString(rest) // any separator character
		if hour, minute, sec, micro, offset, aware, ok = parseISOTimeOffset(rest[size:]); !ok {
			return 0, false
		}
	}
	extraDay := 0
	if hour == 24 {
		if minute != 0 || sec != 0 || micro != 0 {
			return 0, false
		}
		hour, extraDay = 0, 1
	}

	if aware {
		t := time.Date(year, time.Month(month), day+extraDay, hour, minute, sec, 0, time.UTC)
		totalMicros := t.Unix()*1_000_000 + int64(micro) - int64(offset)
		// Python: timedelta.total_seconds() is an exactly rounded
		// integer division by 10**6.
		secs, _ := new(big.Rat).SetFrac64(totalMicros, 1_000_000).Float64()
		return int64(secs * 1000), true
	}
	t := time.Date(year, time.Month(month), day+extraDay, hour, minute, sec, 0, time.Local)
	secs := float64(t.Unix()) + float64(float64(micro)/1e6)
	return int64(secs * 1000), true
}

// parseISODate parses the extended (YYYY-MM-DD) or basic (YYYYMMDD) date at
// the start of ts and returns the rest of ts. The date must exist in the
// proleptic Gregorian calendar.
func parseISODate(ts string) (year, month, day int, rest string, ok bool) {
	var ok1, ok2, ok3 bool
	switch {
	case len(ts) >= 10 && ts[4] == '-' && ts[7] == '-':
		year, ok1 = atoiDigits(ts[0:4])
		month, ok2 = atoiDigits(ts[5:7])
		day, ok3 = atoiDigits(ts[8:10])
		rest = ts[10:]
	case len(ts) >= 8:
		year, ok1 = atoiDigits(ts[0:4])
		month, ok2 = atoiDigits(ts[4:6])
		day, ok3 = atoiDigits(ts[6:8])
		rest = ts[8:]
	default:
		return 0, 0, 0, "", false
	}
	if !ok1 || !ok2 || !ok3 ||
		year < 1 || month < 1 || month > 12 || day < 1 || day > daysIn(year, month) {
		return 0, 0, 0, "", false
	}
	return year, month, day, rest, true
}

// parseISOTimeOffset parses the time part of a timestamp (see parseISOTime)
// and its optional ±offset, returned in microseconds east of UTC; aware
// reports whether there is an offset.
func parseISOTimeOffset(s string) (hour, minute, sec, micro, offset int, aware, ok bool) {
	timePart, tzPart, sign := s, "", 0
	if i := strings.IndexByte(s, '-'); i >= 0 {
		timePart, tzPart, sign = s[:i], s[i+1:], -1
	} else if i := strings.IndexByte(s, '+'); i >= 0 {
		timePart, tzPart, sign = s[:i], s[i+1:], 1
	}
	if hour, minute, sec, micro, ok = parseISOTime(timePart); !ok {
		return 0, 0, 0, 0, 0, false, false
	}
	if sign == 0 {
		return hour, minute, sec, micro, 0, false, true
	}
	if n := len(tzPart); n == 0 || n == 1 || n == 3 {
		return 0, 0, 0, 0, 0, false, false
	}
	oh, om, os_, ous, ok := parseISOTime(tzPart)
	if !ok || oh > 23 {
		return 0, 0, 0, 0, 0, false, false
	}
	offset = sign * ((oh*3600+om*60+os_)*1_000_000 + ous)
	return hour, minute, sec, micro, offset, true, true
}

// parseISOTime parses HH[:MM[:SS[.f+]]] or HHMM[SS[.f+]]; a fraction is
// only accepted after seconds. Hour 24 is returned as-is for the caller to
// validate.
func parseISOTime(s string) (hour, minute, sec, micro int, ok bool) {
	if len(s) < 2 {
		return 0, 0, 0, 0, false
	}
	if hour, ok = atoiDigits(s[:2]); !ok || hour > 24 {
		return 0, 0, 0, 0, false
	}
	s = s[2:]
	extended := strings.HasPrefix(s, ":")
	parsed := 0
	for _, field := range []*int{&minute, &sec} {
		if s == "" || s[0] == '.' || s[0] == ',' {
			break
		}
		if extended {
			if s[0] != ':' {
				return 0, 0, 0, 0, false
			}
			s = s[1:]
		}
		if len(s) < 2 {
			return 0, 0, 0, 0, false
		}
		if *field, ok = atoiDigits(s[:2]); !ok {
			return 0, 0, 0, 0, false
		}
		s = s[2:]
		parsed++
	}
	if minute > 59 || sec > 59 {
		return 0, 0, 0, 0, false
	}
	if s != "" {
		if parsed < 2 || (s[0] != '.' && s[0] != ',') {
			return 0, 0, 0, 0, false
		}
		frac := s[1:]
		if _, ok := atoiDigits(frac); !ok {
			return 0, 0, 0, 0, false
		}
		micro, _ = strconv.Atoi((frac + "000000")[:6])
	}
	return hour, minute, sec, micro, true
}

// atoiDigits parses a string of ASCII digits.
func atoiDigits(s string) (int, bool) {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, s != ""
}

// daysIn returns the number of days in month of year (proleptic Gregorian).
func daysIn(year, month int) int {
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
