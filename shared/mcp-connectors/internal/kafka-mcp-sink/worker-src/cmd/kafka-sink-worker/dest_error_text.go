package main

import (
	"fmt"
	"strings"
)

// destErrorText is the reason a destination gave for refusing a write, read
// from whichever of the three reply shapes carries it (dest_error_text_test.go):
// an "error" string, a JSON-RPC "error" object, or an "errors" list of strings
// or {"message": …} items. A refusal with none of them says so — never "<nil>".
func destErrorText(res map[string]interface{}) string {
	switch e := res["error"].(type) {
	case string:
		if strings.TrimSpace(e) != "" {
			return e
		}
	case map[string]interface{}:
		if msg := errorItemText(e); msg != "" {
			return msg
		}
	case nil:
	default:
		return fmt.Sprint(e)
	}
	if list, ok := res["errors"].([]interface{}); ok && len(list) > 0 {
		seen := map[string]bool{}
		var distinct []string
		for _, item := range list {
			msg := ""
			switch v := item.(type) {
			case string:
				msg = strings.TrimSpace(v)
			case map[string]interface{}:
				msg = errorItemText(v)
			default:
				msg = fmt.Sprint(v)
			}
			if msg != "" && !seen[msg] {
				seen[msg] = true
				distinct = append(distinct, msg)
			}
		}
		if len(distinct) > 3 {
			distinct = append(distinct[:3], "…")
		}
		if len(distinct) > 0 {
			if len(list) == 1 {
				return distinct[0]
			}
			return fmt.Sprintf("%d errors: %s", len(list), strings.Join(distinct, "; "))
		}
	}
	if msg, ok := res["message"].(string); ok && strings.TrimSpace(msg) != "" {
		return msg
	}
	return "the destination refused the write and gave no reason"
}

func errorItemText(item map[string]interface{}) string {
	for _, k := range []string{"message", "error", "detail"} {
		if s, ok := item[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// destToolMissing reports whether a refusal says the TOOL does not exist — the
// only case in which a write may be retried as <dest>_import_data. These are the
// three replies a connector gives for a tool it lacks: BaseMCPConnector's
// "Unknown tool: X", a hand-written server's "tool not found", and the MCP
// server's JSON-RPC "Method not found". A bare "not found" is a destination's
// answer about a ROW (an item route's 404), and re-sending the batch as an
// append duplicates it.
func destToolMissing(res map[string]interface{}) bool {
	msg := strings.ToLower(destErrorText(res))
	return strings.Contains(msg, "unknown tool") ||
		strings.Contains(msg, "tool not found") ||
		strings.Contains(msg, "method not found")
}
