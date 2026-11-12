package agentgw

import (
	"fmt"
	"sort"
	"strings"

	"api-gateway/internal/services"
)

// Markers around the generated part of docs/api/agent-gateway.md. Everything
// between them is RenderDocs' output; everything outside is written by hand.
const (
	DocsBegin = "<!-- BEGIN GENERATED: agent gateway tools (go test ./internal/agentgw -run TestReferenceDocIsCurrent -update) -->"
	DocsEnd   = "<!-- END GENERATED: agent gateway tools -->"
)

// RenderDocs renders the tool reference: the protocol versions, then each
// tool's arguments, what it needs, and every key it can return. It reads the
// same registry, policy tables and Output types the server enforces, so the
// page describes what a call can actually do.
func RenderDocs(tools []*Tool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Protocol versions, newest first: %s.\n", codeList(protocolVersions))
	for _, t := range tools {
		fmt.Fprintf(&b, "\n### `%s`: %s\n\n%s\n\n", t.Name, t.Title, t.Description)
		if len(t.Args) == 0 {
			b.WriteString("No arguments.\n\n")
		} else {
			b.WriteString("| Argument | Type | Required | Description |\n|---|---|---|---|\n")
			for _, a := range t.Args {
				argRow(&b, a.Name, a)
				// An array's item fields follow it, as name[].field.
				for _, f := range a.Items {
					argRow(&b, a.Name+"[]."+f.Name, f)
				}
			}
			b.WriteString("\n")
		}
		b.WriteString(needs(t.Action) + "\n\n")
		if t.Write {
			b.WriteString("**Writes.** This tool changes rsync.ai. ")
			if scope, _ := services.AgentScope(t.Action); services.AdminScope(scope) {
				b.WriteString("Only a workspace owner or admin can give a token its scope.\n\n")
			} else {
				b.WriteString("Any member can give a token its scope.\n\n")
			}
		}
		fmt.Fprintf(&b, "Returns only these keys: %s.\n", codeList(t.Returns()))
	}
	return b.String()
}

func argRow(b *strings.Builder, name string, a Arg) {
	req := "no"
	if a.Required {
		req = "yes"
	}
	fmt.Fprintf(b, "| `%s` | %s | %s | %s |\n", name, argType(a), req, cell(argNotes(a)))
}

func argType(a Arg) string {
	if a.Format != "" {
		return a.Type + " (" + a.Format + ")"
	}
	if a.Type == "array" {
		return fmt.Sprintf("array of at most %d objects", a.MaxItems)
	}
	return a.Type
}

func argNotes(a Arg) string {
	notes := a.Description
	if len(a.Enum) > 0 {
		notes += " One of " + codeList(a.Enum) + "."
	}
	if len(a.Synonyms) > 0 {
		words := make([]string, 0, len(a.Synonyms))
		for w := range a.Synonyms {
			words = append(words, w)
		}
		sort.Strings(words)
		notes += " Also accepts " + codeList(words) + ", case and spacing ignored."
	}
	if a.MaxLength > 0 {
		notes += fmt.Sprintf(" At most %d characters.", a.MaxLength)
	}
	return notes
}

func needs(action services.Action) string {
	role, _ := services.MinRole(action)
	scope, _ := services.AgentScope(action)
	return fmt.Sprintf("Needs the token scope `%s` and the workspace role `%s` or higher.", scope, role)
}

func codeList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = "`" + s + "`"
	}
	return strings.Join(quoted, ", ")
}

// cell escapes a table cell: an unescaped | would split it.
func cell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
