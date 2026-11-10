// Package mask decides which result columns a preview shows masked, by column name:
// the Explorer, charts, dashboards, table samples and Ask rsync.ai's sample rows all
// read the same rule.
package mask

import (
	"fmt"
	"strings"
)

// IsSecretColumnName checks if a column name likely contains secrets/credentials.
// These should always be redacted in previews/Explorer results.
func IsSecretColumnName(colName string) bool {
	lowerName := strings.ToLower(strings.TrimSpace(colName))
	secretKeywords := []string{
		"password", "passwd", "pwd",
		"secret", "client_secret",
		"token", "access_token", "refresh_token", "id_token",
		"api_key", "apikey",
		"private_key", "secret_key",
	}
	for _, keyword := range secretKeywords {
		if strings.Contains(lowerName, keyword) {
			return true
		}
	}
	return false
}

// ambiguousPIIKeywords are the PII keywords short enough to appear INSIDE
// ordinary English words, so they are matched per-token instead of as bare
// substrings. Every one of these had a real false positive: "tin" fires on
// dis-TIN-ct / rou-TIN-g / set-TIN-g, "nin" on run-NIN-g / war-NIN-g, "sin" on
// u-SIN-g / bu-SIN-ess, "pan" on com-PAN-y / ex-PAN-sion, "cell" on ex-CELL-ent.
// `SELECT COUNT(DISTINCT id) AS distinct_ids` came back as `***`, which is what
// surfaced this.
var ambiguousPIIKeywords = map[string]bool{
	"tin": true, "nin": true, "sin": true, "pan": true, "cell": true,
}

// ColumnNameTokens splits a column name into lowercase word tokens, breaking on
// separators, digits, and camelCase transitions: "customerPAN2" → [customer pan 2],
// "distinct_ids" → [distinct ids].
func ColumnNameTokens(colName string) []string {
	var tokens []string
	var cur []rune
	runes := []rune(strings.TrimSpace(colName))
	flush := func() {
		if len(cur) > 0 {
			tokens = append(tokens, strings.ToLower(string(cur)))
			cur = nil
		}
	}
	for i, r := range runes {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			// lower→upper marks a camelCase boundary ("customerPan").
			if r >= 'A' && r <= 'Z' && i > 0 && runes[i-1] >= 'a' && runes[i-1] <= 'z' {
				flush()
			}
			cur = append(cur, r)
		case r >= '0' && r <= '9':
			flush()
		default:
			flush()
		}
	}
	flush()
	return tokens
}

// IsPIIColumnName checks if a column name likely contains PII.
// Keep this focused on true PII fields (email/phone/id numbers) and avoid generic "id"/"*_id".
//
// This drives PREVIEW display only, and it is keyed on the RESULT column name —
// `SELECT email AS x` already renders unredacted, and the caller can query the
// connection freely in the first place. So it is a legibility guard, not an
// access control: a false positive (an aggregate rendered as `***`) costs more
// than the marginal case it would catch.
func IsPIIColumnName(colName string) bool {
	lowerName := strings.ToLower(strings.TrimSpace(colName))
	piiKeywords := []string{
		// Contact
		"email", "e-mail",
		"phone", "mobile", "cell", "msisdn",

		// Government / identity numbers
		"ssn", "social_security", "social-security",
		"national_id", "national-id",
		"id_number", "id-number",
		"passport", "passport_number", "passport-number",
		"driver_license", "driver_licence", "drivers_license", "drivers_licence",
		"tax_id", "tax-id", "tin",
		"aadhar", "aadhaar",
		"nin", "sin",

		// Payments (PII-sensitive)
		"credit_card", "cc_number", "card_number", "pan",
	}

	var tokens []string
	for _, keyword := range piiKeywords {
		if ambiguousPIIKeywords[keyword] {
			if tokens == nil {
				tokens = ColumnNameTokens(colName)
			}
			for _, tok := range tokens {
				if tok == keyword {
					return true
				}
			}
			continue
		}
		if strings.Contains(lowerName, keyword) {
			return true
		}
	}
	return false
}

// ShouldRedactColumnName decides whether to redact a column in previews.
func ShouldRedactColumnName(colName string) bool {
	return IsSecretColumnName(colName) || IsPIIColumnName(colName)
}

// RedactForPreview redacts a value for preview display
func RedactForPreview(value interface{}) string {
	if value == nil {
		return ""
	}
	str := fmt.Sprintf("%v", value)
	if len(str) > 4 {
		return str[:2] + "***"
	}
	return "***"
}
