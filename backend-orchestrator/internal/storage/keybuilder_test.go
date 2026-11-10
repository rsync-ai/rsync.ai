package storage

import (
	"testing"
)

func TestSlugify(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"My Pipeline Name", "my-pipeline-name"},
		{"pipeline_with_underscores", "pipeline-with-underscores"},
		{"Pipeline 123", "pipeline-123"},
		{"  spaces  around  ", "spaces-around"},
		{"UPPERCASE", "uppercase"},
		{"special!@#$%chars", "specialchars"},
		{"multiple---hyphens", "multiple-hyphens"},
		{"", ""},
		{"a", "a"},
		{"ab", "ab"},
		{"test-pipeline", "test-pipeline"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := Slugify(tt.input)
			if result != tt.expected {
				t.Errorf("Slugify(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestSanitizePath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"normal_table", "normal_table"},
		{"schema/table", "schema_table"},
		{"path\\with\\backslash", "path_with_backslash"},
		{"../parent", "__parent"},
		{"table:name", "table_name"},
		{"  spaces  ", "spaces"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := SanitizePath(tt.input)
			if result != tt.expected {
				t.Errorf("SanitizePath(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestRunMode(t *testing.T) {
	tests := []struct {
		input    string
		expected RunMode
	}{
		{"resume", RunModeResume},
		{"Resume", RunModeResume},
		{"RESUME", RunModeResume},
		{"reload", RunModeReload},
		{"Reload", RunModeReload},
		{"RELOAD", RunModeReload},
		{"", RunModeResume},
		{"invalid", RunModeResume},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := ParseRunMode(tt.input)
			if result != tt.expected {
				t.Errorf("ParseRunMode(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestExtractSchemaAndTable(t *testing.T) {
	tests := []struct {
		input      string
		wantSchema string
		wantTable  string
	}{
		{"users", "", "users"},
		{"public.users", "public", "users"},
		{"mydb.public.users", "public", "users"},
		{"a.b.c.d", "c", "d"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			schema, table := ExtractSchemaAndTable(tt.input)
			if schema != tt.wantSchema || table != tt.wantTable {
				t.Errorf("ExtractSchemaAndTable(%q) = (%q, %q), want (%q, %q)",
					tt.input, schema, table, tt.wantSchema, tt.wantTable)
			}
		})
	}
}
