// Package compat holds the JavaScript string semantics shared by package
// claude and package sessions, which must match the reference SDKs byte for
// byte.
package compat

import "strings"

// isJSSpace reports whether JavaScript's String.prototype.trim strips r.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// JSTrim is JavaScript's String.prototype.trim.
func JSTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }

// EnvTruthy reports whether an environment value is 1, true, yes or on
// (case-insensitive, surrounding whitespace ignored).
func EnvTruthy(v string) bool {
	switch strings.ToLower(JSTrim(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
