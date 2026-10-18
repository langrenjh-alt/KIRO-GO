package config

import (
	"regexp"
	"strings"
)

// modelVersionDash matches a numeric version written with a hyphen, such as
// the "4-8" in "claude-opus-4-8". The minor is limited to one or two digits
// and must not continue into a longer number, so a dated snapshot
// ("...-20250514") is not rewritten into a minor version.
var modelVersionDash = regexp.MustCompile(`(\d+)-(\d{1,2})(?:\b|$)`)

// modelVersionDot matches the same version once it is written with a dot.
var modelVersionDot = regexp.MustCompile(`(\d+)\.(\d{1,2})(?:\b|$)`)

// modelTrailingDate matches an Anthropic-style snapshot suffix. It is removed
// before version normalization so "claude-opus-4-8-20260101" resolves to the
// same id as "claude-opus-4.8".
var modelTrailingDate = regexp.MustCompile(`-20\d{6}$`)

// CanonicalModelID folds version-separator and dated-snapshot spellings onto
// one id. Kiro publishes dotted versions ("claude-opus-4.8") while clients
// often send the dashed Anthropic id ("claude-opus-4-8"); both must select
// the same account and upstream model.
func CanonicalModelID(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return ""
	}
	model = modelTrailingDate.ReplaceAllString(model, "")
	return modelVersionDash.ReplaceAllString(model, "${1}.${2}")
}

// PublicModelID is the spelling published by /v1/models. Kiro's catalog uses
// dots ("claude-opus-5.5"), and the upstream rejects the hyphenated minor, so
// routing keeps CanonicalModelID. Clients only need one list entry.
func PublicModelID(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return ""
	}
	model = modelTrailingDate.ReplaceAllString(model, "")
	return modelVersionDot.ReplaceAllString(model, "${1}-${2}")
}

// AlternateModelID returns the other accepted spelling of a versioned model
// id. An empty result means the id has no numeric version pair to rewrite.
func AlternateModelID(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	lower := strings.ToLower(model)
	canonical := CanonicalModelID(lower)
	if canonical != "" && canonical != lower {
		return canonical
	}
	reversed := modelVersionDot.ReplaceAllString(lower, "${1}-${2}")
	if reversed == lower {
		return ""
	}
	return reversed
}
