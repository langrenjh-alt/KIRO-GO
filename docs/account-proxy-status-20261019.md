# v1.1.9: account eligibility and proxy status

## Fixes

- Account API reports `proxyBlocked` only for an enabled account without a
  terminal BANNED/DISABLED/SUSPENDED state. Unbinding a disabled account is not
  evidence that the proxy pool has no usable proxies.
- Account cards distinguish banned/unbound, disabled/unassigned, and the existing
  explicit authentication-invalid reason. Only an eligible account genuinely
  blocked by the proxy-pool setting retains the proxy-failure styling.
- Background UI updates retain account eligibility and reason fields; partial
  responses do not erase previously known state.
- `RefreshAccountInfo` now checks typed transport errors before matching
  authentication wording. CONNECT 401/403 and SOCKS authentication failures are
  reported as network errors, not used to ban an account. Actual AWS HTTP
  authentication failures and suspension responses keep their existing behavior.

## Verification

- New account API regression: 15 cases.
- New account-card and polling regression: 23 cases.
- New refresh regression: 5 transport scenarios and 4 real HTTP business-error
  scenarios, all using isolated local fixtures rather than production credentials.
- Before the fix, tests reproduced incorrect proxy blocking for 9 inactive
  account cases, 20 UI assertions, and 3 refresh transport cases.
- Full Go packages, race checks, vet, existing proxy UI/pagination regressions,
  binary health/API fixtures, and program-only rollback are release gates.

## Diagnostic boundary

The observed deployment had three accounts with explicit AWS user-suspension
responses and one with an explicit invalid-bearer-token response. Six anonymous
TLS-verified requests through three healthy pool proxies reached the AWS service
and returned its expected missing-token response. This distinguishes current
network reachability from account eligibility; it does not establish what
historically triggered AWS risk controls.

This update does not reactivate accounts, change proxy bindings, overwrite live
configuration, or claim to resolve the separate CCTest structured-output score.
