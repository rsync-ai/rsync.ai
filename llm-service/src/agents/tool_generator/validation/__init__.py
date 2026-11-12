"""
Validation Package

Provides structured validation for connector generation:
- Pre-generation: Validate ConnectorSpec before code generation
- Post-generation: Validate generated code
- Category checks: contract-driven code checks per connector category

VERSION: 1.0.0
"""

from .models import ValidationError, ValidationResult, ValidationSeverity
from .pre_generation import PreGenerationValidator
from .post_generation import PostGenerationValidator
from .security_lint import (
    DockerfileFieldRejected,
    SecurityFinding,
    lint_connector_code,
    sanitize_dockerfile_field,
    sanitize_spec_for_dockerfile,
)

__all__ = [
    "ValidationError",
    "ValidationResult",
    "ValidationSeverity",
    "PreGenerationValidator",
    "PostGenerationValidator",
    "DockerfileFieldRejected",
    "SecurityFinding",
    "lint_connector_code",
    "sanitize_dockerfile_field",
    "sanitize_spec_for_dockerfile",
]

