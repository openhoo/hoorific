# Security policy

## Reporting a vulnerability

Please do not disclose suspected vulnerabilities, credentials, exploit details,
or sensitive evidence in a public issue, discussion, or pull request.

When GitHub private vulnerability reporting is enabled for this repository, use
[GitHub's private vulnerability reporting form](https://github.com/openhoo/hoorific/security/advisories/new).
If the form is unavailable, do not substitute a public issue; retain the report
until the maintainers publish a private channel.

Include only the information needed to reproduce and assess the report:

- affected commit, version, image digest, or deployment;
- affected component and configuration;
- impact and prerequisites;
- minimal reproduction steps and safe proof of concept;
- suggested mitigation, if known; and
- whether the issue is already public.

Do not send live secrets, production data, private endpoints, browser storage
state, database files, or unredacted provider responses. Use synthetic data and
redact tokens, account identifiers, cookies, and user content.

## Maintainer handling

Maintainers will keep reports private while validating impact, identify affected
versions, prepare a fix and regression coverage, and coordinate disclosure
through a private GitHub advisory when appropriate. This project has no
published response-time SLA or supported-version promise yet; disclosure timing
and support scope are decided from the validated report.

## Scope

Reports may cover the Hoorific gateway, management console, SDK-facing protocol
surface, configuration and credential handling, qualification tooling, and the
container image in this repository. Provider accounts, third-party services,
and issues requiring live credentials should include a minimal local
reproduction where possible.
