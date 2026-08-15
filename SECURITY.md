# Security policy

## Supported versions

Security fixes are applied to the latest release on the default branch. QuietFeed has not yet reached a stable 1.0 release, so older revisions may not receive security updates.

## Reporting a vulnerability

Please do not open a public issue for a suspected vulnerability. Use GitHub's private vulnerability reporting for this repository:

https://github.com/hxueh/quietfeed/security/advisories/new

Include the affected version or commit, reproduction steps, expected impact, and any suggested mitigation. Please allow reasonable time for investigation and a fix before public disclosure.

## Security model

QuietFeed is a single-user service intended to run behind an HTTPS reverse proxy. Anyone with the configured username and password can manage subscriptions and cause the server to fetch permitted feed URLs. Private and local network destinations are blocked by default.
