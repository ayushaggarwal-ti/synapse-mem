# Security

smem signs and verifies its log, quarantines untrusted synced data, redacts
secrets, and keeps code execution out of committed config. If you find a way
around any of that, or any other vulnerability, please report it privately:
use **Report a vulnerability** on the repository's Security tab (GitHub
private vulnerability reporting). Don't open a public issue.

Please include the version (`smem version`), what you did, what happened,
and what you expected. You'll get an acknowledgement within a few days.

`docs/DESIGN.md` §7 describes what the signatures do and don't guarantee:
the log is tamper-evident, not tamper-proof.
