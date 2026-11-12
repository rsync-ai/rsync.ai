package validators

import "github.com/rsync-ai/shared/sqlexec/stmt"

// The SQL classifier lives in shared/go/sqlexec/stmt, beside the engines that run
// what it classifies (query-service-design.md § 6). These names keep the gateway's
// callers and the role check (statement_policy.go) unchanged.

type (
	SQLValidationResult = stmt.SQLValidationResult
	StatementClass      = stmt.StatementClass
	TableRef            = stmt.TableRef
)

const (
	ErrCodeMultipleStatements = stmt.ErrCodeMultipleStatements
	ErrCodeEmptyQuery         = stmt.ErrCodeEmptyQuery

	ClassRead        = stmt.ClassRead
	ClassDMLWrite    = stmt.ClassDMLWrite
	ClassDDL         = stmt.ClassDDL
	ClassDestructive = stmt.ClassDestructive
	ClassBlocked     = stmt.ClassBlocked
	ClassUnknown     = stmt.ClassUnknown
)

var (
	ValidateExplorerSQL    = stmt.ValidateExplorerSQL
	ClassifyStatementSQL   = stmt.ClassifyStatementSQL
	IsWriteClass           = stmt.IsWriteClass
	ClassifyExecutionError = stmt.ClassifyExecutionError
	ExtractTableReferences = stmt.ExtractTableReferences
	ExtractWriteTargets    = stmt.ExtractWriteTargets
	IsSingleStatement      = stmt.IsSingleStatement
)
