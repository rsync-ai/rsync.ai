package validators

import (
	"fmt"
	"strings"

	"api-gateway/internal/security"

	"github.com/rsync-ai/shared/sqlexec/stmt"
)

// Additional error codes for the role-aware Explorer statement validator.
const (
	// ErrCodeInsufficientRole is returned when the statement class is runnable but
	// the caller's workspace role is below the class minimum (classToMinRole).
	ErrCodeInsufficientRole = "INSUFFICIENT_ROLE_FOR_STATEMENT"
	// ErrCodeStatementBlocked is returned for statement classes no role may run via
	// the Data Explorer (GRANT/REVOKE/CALL/EXEC/SET/…).
	ErrCodeStatementBlocked = "STATEMENT_NOT_ALLOWED"
)

// classToMinRole is the single source of truth mapping a statement class to the
// minimum workspace role permitted to run it in the Data Explorer:
//   - viewer/member: read only (Explorer is unchanged for them)
//   - admin: DML writes + non-destructive DDL (INSERT/UPDATE/DELETE/MERGE/CREATE/ALTER)
//   - owner: + destructive (DROP/TRUNCATE)
//
// ClassBlocked is absent on purpose — no role may run those via Explorer.
// ClassUnknown maps to owner so an unrecognized statement fails closed (owner-only)
// rather than silently running at a lower tier.
var classToMinRole = map[StatementClass]security.WorkspaceRole{
	ClassRead:        security.WSViewer,
	ClassDMLWrite:    security.WSAdmin,
	ClassDDL:         security.WSAdmin,
	ClassDestructive: security.WSOwner,
	ClassUnknown:     security.WSOwner,
}

// MinRoleForClass returns the minimum workspace role for a class and whether the
// class is runnable at all. blocked=true means no role may run it via Explorer.
func MinRoleForClass(class StatementClass) (min security.WorkspaceRole, blocked bool) {
	min, ok := classToMinRole[class]
	if !ok {
		return "", true
	}
	return min, false
}

// MaxClassForRole is the highest statement class a role may run in the Data Explorer,
// read off classToMinRole. The gateway passes it to the engine as the class a write
// may not exceed; the engine classifies the statement again (stmt.Permits).
func MaxClassForRole(role security.WorkspaceRole) StatementClass {
	switch {
	case role.Meets(classToMinRole[ClassDestructive]):
		return ClassDestructive
	case role.Meets(classToMinRole[ClassDMLWrite]):
		return ClassDMLWrite
	default:
		return ClassRead
	}
}

// ValidateExplorerStatement validates a Data Explorer statement for a caller with
// the given workspace role. Reads (SELECT/WITH) keep the full historical SELECT-only
// safety pipeline (ValidateExplorerSQL) unchanged. Writes are permitted only when the
// caller's role meets the class minimum: DML/DDL require admin, destructive
// (DROP/TRUNCATE) requires owner. Blocked classes (GRANT/REVOKE/CALL/…) and
// unrecognized statements fail closed. A single statement is required for every class.
func ValidateExplorerStatement(sqlText string, role security.WorkspaceRole) *SQLValidationResult {
	trimmed := strings.TrimSpace(sqlText)
	if trimmed == "" {
		return invalidStatement(ErrCodeEmptyQuery, "Query cannot be empty", "")
	}

	stripped := strings.TrimSpace(stmt.RemoveComments(trimmed))
	if stripped == "" {
		return invalidStatement(ErrCodeEmptyQuery, "Query cannot be empty (only comments)", "")
	}

	// Single statement only — for every class. Blocks stacked-statement attacks
	// (e.g. `UPDATE …; DROP TABLE …`) before the class/role gate.
	if stmt.HasMultipleStatements(stripped) {
		return invalidStatement(ErrCodeMultipleStatements, "Multiple SQL statements are not allowed", "")
	}

	stmtType := stmt.IdentifyStatementType(stripped)
	// Classify on the comment-stripped statement, not just its verb, so
	// `ALTER … DROP COLUMN` is gated as destructive (owner) rather than as DDL (admin).
	class := ClassifyStatementSQL(stripped)

	// Reads keep the exact historical behavior (SELECT-only pipeline: dangerous
	// keyword + injection heuristics, LIMIT/table extraction, warnings). Reads are
	// gated by workspace membership at the middleware, not by a role tier here.
	if class == ClassRead {
		return ValidateExplorerSQL(sqlText)
	}

	minRole, blocked := MinRoleForClass(class)
	if blocked {
		return invalidStatement(ErrCodeStatementBlocked,
			fmt.Sprintf("%s statements are not allowed in the Data Explorer", stmtType), stmtType)
	}
	if !role.Meets(minRole) {
		return invalidStatement(ErrCodeInsufficientRole,
			fmt.Sprintf("This statement requires the %s role. Found: %s", minRole, stmtType), stmtType)
	}

	// Authorized write / DDL / destructive statement. The dangerous-keyword and
	// injection heuristics are intentionally skipped here: they exist to block exactly
	// these statements on the read path, and the role gate above is the authorization.
	// The single-statement guard already ran.
	return &SQLValidationResult{
		Valid:            true,
		StatementType:    stmtType,
		Warnings:         []string{},
		ReferencedTables: stmt.ExtractTables(stripped),
	}
}

func invalidStatement(code, msg, stmtType string) *SQLValidationResult {
	return &SQLValidationResult{
		Valid:         false,
		ErrorCode:     code,
		ErrorMessage:  msg,
		StatementType: stmtType,
	}
}
