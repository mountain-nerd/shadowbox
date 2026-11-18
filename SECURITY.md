# Security policy

shadowbox sits in the production request path and decides whether a rollout may proceed, so a bug that lets a candidate cause side effects, leaks production credentials or prompts, or lets someone force a `pass` is a security issue.

## Reporting a vulnerability

Please **do not open a public issue.** Report it privately through GitHub: open the repository's **Security** tab and choose **Report a vulnerability**. Include what you found, how to reproduce it, and which version or commit you tested.

You can expect an acknowledgement within a week. This is a single-maintainer project, so fixes are best-effort; you will be kept informed and credited in the advisory unless you prefer otherwise.

## Supported versions

shadowbox is at v0.1 and experimental. Only the latest commit on `main` receives fixes.

## In scope

- the shadow copy reaching the candidate with a production credential, a provider-run tool, provider-side state, or any field or header outside the allowlists
- a client or another workload changing the role, the experiment, the pairing key or the verdict
- prompt text or tool arguments being stored or exposed through metrics or logs
- the live path being slowed, altered or failed by shadowbox

Deployment choices documented as your responsibility (network policy, mTLS between the gateways and shadowbox, secret management) are out of scope, but reports that the documentation is wrong or unclear are welcome.
