# Security Policy

## Supported versions

This project is currently pre-release. Until a stable release is published,
only the latest code on the default branch should be considered for security
fixes.

## Reporting a vulnerability

Please do not open a public issue for a suspected security vulnerability.

Use GitHub's private vulnerability reporting feature for this repository where
available. Include enough information to reproduce and assess the issue, but
avoid including production credentials, access tokens, personal data, or other
secrets.

## Scope

This module controls request admission within a Caddy process. It is not a
security boundary between operating-system users, containers, hosts, or Caddy
processes, and it does not provide distributed quotas.

A bypass which allows requests to execute the protected handler chain beyond
the configured local concurrency limit, a counter leak which can permanently
deny service, unsafe handling of untrusted request data, or a remotely
triggerable crash is considered security-relevant.
