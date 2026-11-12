// Package storage provides utilities for cloud storage operations
package storage

import (
	"strings"
	"unicode"
)

// Slugify converts a string to a URL/filesystem-safe slug
// Used for converting pipeline names to dataset namespaces
func Slugify(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// Convert to lowercase
	s = strings.ToLower(s)

	// Replace common separators with hyphens
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "_", "-")

	// Keep only alphanumeric and hyphens
	var result strings.Builder
	lastWasHyphen := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			result.WriteRune(r)
			lastWasHyphen = false
		} else if r == '-' && !lastWasHyphen && result.Len() > 0 {
			result.WriteRune('-')
			lastWasHyphen = true
		}
	}

	// Trim trailing hyphens
	return strings.TrimRight(result.String(), "-")
}

// SanitizePath removes or replaces characters that could cause path traversal or issues
func SanitizePath(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// Replace path separators and dangerous characters
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, "..", "_")
	s = strings.ReplaceAll(s, ":", "_")

	// Remove control characters
	var result strings.Builder
	for _, r := range s {
		if unicode.IsPrint(r) && r != '\n' && r != '\r' && r != '\t' {
			result.WriteRune(r)
		}
	}

	return result.String()
}

// RunMode represents the sync execution mode
type RunMode string

const (
	// RunModeResume continues from checkpoints/watermarks without duplication (default)
	RunModeResume RunMode = "resume"
	// RunModeReload rebuilds destination from scratch (capability gated)
	RunModeReload RunMode = "reload"
)

// ParseRunMode parses a string to RunMode, defaulting to resume
func ParseRunMode(s string) RunMode {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == string(RunModeReload) {
		return RunModeReload
	}
	return RunModeResume
}

// ExtractSchemaAndTable extracts schema/db and table from a qualified table name
// Handles formats like: "schema.table", "db.schema.table", "table"
func ExtractSchemaAndTable(qualifiedName string) (schema, table string) {
	parts := strings.Split(qualifiedName, ".")
	switch len(parts) {
	case 1:
		return "", parts[0]
	case 2:
		return parts[0], parts[1]
	case 3:
		// db.schema.table - use schema as the schema part
		return parts[1], parts[2]
	default:
		// Too many parts, just take last two
		n := len(parts)
		return parts[n-2], parts[n-1]
	}
}
