package domain

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	slugMinLen    = 3
	slugMaxLen    = 28
	nameMaxLength = 200 // in characters (runes), not bytes
)

// slugPattern is the brief's slug rule (k8s-namespace compatible). It also
// enforces the 3-28 length: one leading letter, 1-26 middle characters, one
// trailing letter or digit.
var slugPattern = regexp.MustCompile(`^[a-z][-a-z0-9]{1,26}[a-z0-9]$`)

var (
	errSlugLength  = errors.New("must be 3-28 characters")
	errSlugFormat  = errors.New("must start with a lowercase letter, contain only lowercase letters, digits and '-', and end with a letter or digit")
	errNameEmpty   = errors.New("must not be empty")
	errNameTooLong = errors.New("must be at most 200 characters")
)

// ValidateSlug checks slug against the slug rule. The error is a plain
// message for one field; ValidateCreate turns it into a validation_error.
func ValidateSlug(slug string) error {
	if n := utf8.RuneCountInString(slug); n < slugMinLen || n > slugMaxLen {
		return errSlugLength
	}
	if !slugPattern.MatchString(slug) {
		return errSlugFormat
	}
	return nil
}

// NormalizeName trims surrounding whitespace and checks the result: non-empty
// and at most 200 characters. The trimmed name is what gets stored.
func NormalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errNameEmpty
	}
	if utf8.RuneCountInString(name) > nameMaxLength {
		return "", errNameTooLong
	}
	return name, nil
}

// ValidateCreate checks a new tenant's slug and name together, so the client
// sees every problem at once. It returns the normalized name, or a
// validation_error with one entry per invalid field.
func ValidateCreate(slug, name string) (string, error) {
	fe := FieldErrors{}
	if err := ValidateSlug(slug); err != nil {
		fe.Add("slug", err.Error())
	}
	normalized, err := NormalizeName(name)
	if err != nil {
		fe.Add("name", err.Error())
	}
	if err := fe.Err(); err != nil {
		return "", err
	}
	return normalized, nil
}
