"""A ci.yml report upload runs only when there is a failure to report.

The a11y job's "Upload report on failure" step was gated on `always()`, so it
ran on every run. While the account artifact quota was full
(KI-SECURITY-SARIF-HAS-NOWHERE-TO-LAND) the upload itself failed, and a CLEAN
a11y suite ended in a red X on the nightly. A report is only worth storing when
something failed; an upload on every run spends quota and, when the quota is
gone, invents a failure.

The class: an `actions/upload-artifact` step in ci.yml whose `if:` does not ask
about a failure. security.yml's SARIF archives are deliberately excluded -- they
are the findings' only machine-readable sink, not a debugging report, and their
gating is pinned by test_sarif_findings_have_a_destination.py.
"""

import os

import yaml

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
CI_WORKFLOW = os.path.join(REPO_ROOT, ".github", "workflows", "ci.yml")


def _upload_steps():
    doc = yaml.safe_load(open(CI_WORKFLOW))
    for job_id, job in doc["jobs"].items():
        for step in job.get("steps") or []:
            if str(step.get("uses", "")).startswith("actions/upload-artifact@"):
                yield job_id, step


def test_the_census_finds_the_a11y_report_upload():
    """Anti-vacuity: an empty census passes the check below for free."""
    names = {(job_id, step.get("name")) for job_id, step in _upload_steps()}
    assert any("a11y" in job_id for job_id, _ in names), names


def test_every_report_upload_is_gated_on_a_failure():
    offenders = []
    for job_id, step in _upload_steps():
        cond = str(step.get("if", ""))
        # Either the job failed (`failure()`) or a named step did
        # (`steps.<id>.outcome == 'failure'` -- outcome, because a
        # continue-on-error step's conclusion is always success).
        if "failure()" not in cond and ".outcome == 'failure'" not in cond:
            offenders.append(f"{job_id}: {step.get('name')!r} if: {cond!r}")
    assert offenders == [], (
        "These ci.yml uploads run when nothing failed, spending the account artifact "
        "quota on every run -- and once it is full, failing a clean job: "
        + "; ".join(offenders)
    )
