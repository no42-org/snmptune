# Security

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's private vulnerability reporting: open the **Security** tab of this repository and choose **Report a vulnerability**, or use https://github.com/no42-org/snmptune/security/advisories/new.

Do not open a public issue for a security problem.

You will get an acknowledgement within a week. Fixes ship as a normal release; the advisory is published once the fix is available.

## Scope

snmptune sends read-only SNMP requests to a target the operator names. Reports about the tool loading an agent harder than its safety rules promise are in scope, as are supply-chain concerns about the release pipeline. Weaknesses in the SNMP agents being measured are not.

## Verifying releases

Release checksums are signed with cosign and every artifact carries a build provenance attestation. `RELEASING.md` shows the verification commands.
