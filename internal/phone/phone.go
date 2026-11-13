// Package phone normalizes explicitly international phone numbers.
package phone

import (
	"errors"
	"regexp"
	"strings"
)

var ErrNumber = errors.New("enter an international phone number beginning with +, such as +14155550123")
var e164 = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// Normalize accepts presentation punctuation but never guesses a country code.
func Normalize(value string) (string, error) {
	value = strings.TrimSpace(value)
	value = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", ".", "").Replace(value)
	if !e164.MatchString(value) {
		return "", ErrNumber
	}
	return value, nil
}
