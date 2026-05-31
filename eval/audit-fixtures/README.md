# Audit Fixtures

Shared corpora used by security regression tests added during the 2026-05-03 audit remediation. Each line is one input.

- `cmd_obfuscation.txt` — destructive shell commands obfuscated with quoting, IFS, `\`, ANSI-C, command substitution, here-docs, etc. Tests that any command-classification denylist correctly flags every line.
- `secret_obfuscation.txt` — high-confidence credential examples used by the built-in secret-detection guardrails. Tests assert every listed line is flagged; this is not a claim of exhaustive secret detection.
- `ssrf_urls.txt` — URLs that must be rejected by the SSRF guard (loopback, RFC1918, link-local, IPv6 metadata, embedded credentials, schemes, encodings).
- `mcp_malicious.json` — `.mcp.json` payloads that try to exfiltrate env vars, escape the workspace, or shadow control-flow tools.

Tests should iterate every line and assert the relevant guard fires. Add new lines whenever a bypass is found.
