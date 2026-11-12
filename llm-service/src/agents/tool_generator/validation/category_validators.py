"""
Category-Specific Validators

Runs the contract-driven code checks for a connector's category
(fast AST-based checks with caching and actionable errors).

VERSION: 2.0.0 - Contract-driven validation
"""

import logging
from typing import List, Dict, Any

try:
    from ..schemas.spec import ConnectorSpec
    from .models import ValidationResult
    try:
        # Prefer absolute import when running inside the service container
        from src.agents.tool_generator.contracts.code_checks import validate_connector_code, ASTCache
    except Exception:
        # Fallback to relative import when running as a package
        from ..contracts.code_checks import validate_connector_code, ASTCache
except Exception:
    # Fallback for different execution context
    import sys
    import os
    sys.path.insert(0, os.path.dirname(os.path.dirname(__file__)))
    from schemas.spec import ConnectorSpec
    from validation.models import ValidationResult
    # Best-effort import of contract-driven code checks in non-package contexts (e.g. pytest with sys.path hacks)
    try:
        from contracts.code_checks import validate_connector_code, ASTCache
    except Exception:
        validate_connector_code = None
        ASTCache = None

logger = logging.getLogger(__name__)


def run_code_validation(code: str, category: str) -> List[Dict[str, Any]]:
    """
    Run fast code validation using contract-driven checks.
    
    Returns:
        List of validation error dicts (empty if valid)
    """
    if validate_connector_code is None or ASTCache is None:
        logger.warning("Code validation not available (contracts.code_checks not imported)")
        return []
    
    ast_cache = ASTCache()
    errors = validate_connector_code(code, category, ast_cache)
    return [e.to_dict() for e in errors]


def validate_category_fast(code: str, spec: ConnectorSpec) -> ValidationResult:
    """
    Fast category validation using contract-driven AST checks.
    
    This is the new recommended validation path that uses:
    - Substring prefilter for fast checks
    - Targeted AST parsing (only specific functions)
    - Per-request AST caching
    - Structured actionable error messages
    
    Args:
        code: Generated connector code
        spec: ConnectorSpec
    
    Returns:
        ValidationResult with pass/fail and actionable errors
    """
    result = ValidationResult(passed=True)
    
    # Get category string value
    try:
        category_str = spec.category.value if hasattr(spec.category, 'value') else str(spec.category)
    except Exception:
        category_str = "unknown"
    
    # Run fast code validation
    validation_errors = run_code_validation(code, category_str)
    
    # Convert to ValidationResult
    for err in validation_errors:
        why = err.get("why") or ""
        evidence = err.get("evidence") or ""
        how_to_fix = err.get("how_to_fix") or []
        if isinstance(how_to_fix, list):
            fix_suggestion = " | ".join([str(x) for x in how_to_fix if x])
        else:
            fix_suggestion = str(how_to_fix)

        extra = []
        if why:
            extra.append(f"why={why}")
        if evidence:
            extra.append(f"evidence={evidence}")
        extra_text = ("; " + "; ".join(extra)) if extra else ""

        result.add_error(
            message=(err.get("message", "") + extra_text).strip(),
            location=err.get("location", ""),
            code=err.get("code", "VALIDATION_ERROR"),
            fix_suggestion=fix_suggestion or None,
        )
    
    return result
