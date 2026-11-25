package auth

import (
	"regexp"
	"strings"
)

var (
	phoneCharsRe = regexp.MustCompile(`^[\d\s()+\-.]*$`)
	nonDigitRe   = regexp.MustCompile(`\D`)
	mobileRe     = regexp.MustCompile(`^9\d{9}$`)
	landlineRe   = regexp.MustCompile(`^0\d{5,9}$`)
)

// NormalisePhone returns the one stored form of a Nepali phone (mirrors the web's
// normalisePhone): digits only, without the +977 country code. Mobiles are 10
// digits starting with 9; landlines start with 0. "" means no phone; ok=false
// means it isn't a Nepali number.
func NormalisePhone(in string) (string, bool) {
	raw := strings.TrimSpace(in)
	if raw == "" {
		return "", true
	}
	if !phoneCharsRe.MatchString(raw) {
		return "", false
	}
	digits := nonDigitRe.ReplaceAllString(raw, "")
	international := false
	switch {
	case strings.HasPrefix(digits, "00977"):
		digits, international = digits[5:], true
	case strings.HasPrefix(digits, "977") && len(digits) > 9:
		digits, international = digits[3:], true
	}
	// +977 1 4412345: a landline written internationally drops its trunk 0; put it back.
	if international && !strings.HasPrefix(digits, "9") && !strings.HasPrefix(digits, "0") {
		digits = "0" + digits
	}
	if mobileRe.MatchString(digits) || landlineRe.MatchString(digits) {
		return digits, true
	}
	return "", false
}
