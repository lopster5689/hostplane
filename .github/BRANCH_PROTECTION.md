# Branch Protection — `main`

Repo: `lopster568/hostplane`

## Required settings

Settings → Branches → Branch protection rule for `main`:

- [x] **Require a pull request before merging** (no required reviews — solo dev)
- [x] **Require status checks to pass before merging**
  - [x] Require branches to be up to date before merging
  - Status checks: `build + vet + unit`, `integration (Docker + MariaDB)`
- [x] **Do not allow bypassing the above settings**
- [ ] Restrict pushes that create matching branches *(leave off)*
- [x] **Allow force pushes:** OFF
- [x] **Allow deletions:** OFF

## Apply via gh CLI

```bash
gh api -X PUT repos/lopster568/hostplane/branches/main/protection --input - <<'JSON'
{
  "required_status_checks": {
    "strict": true,
    "contexts": ["build + vet + unit", "integration (Docker + MariaDB)"]
  },
  "enforce_admins": true,
  "required_pull_request_reviews": {
    "required_approving_review_count": 0,
    "dismiss_stale_reviews": false,
    "require_code_owner_reviews": false
  },
  "restrictions": null,
  "allow_force_pushes": false,
  "allow_deletions": false,
  "required_linear_history": false,
  "required_conversation_resolution": false
}
JSON
```

If you later add a contributor, set `required_approving_review_count` to 1.

## Sanity-check after applying

```bash
gh api repos/lopster568/hostplane/branches/main/protection | jq '{
  contexts: .required_status_checks.contexts,
  strict: .required_status_checks.strict,
  enforce_admins: .enforce_admins.enabled,
  force_pushes: .allow_force_pushes.enabled,
  deletions: .allow_deletions.enabled
}'
```

Expected:
```json
{
  "contexts": ["build + vet + unit", "integration (Docker + MariaDB)"],
  "strict": true,
  "enforce_admins": true,
  "force_pushes": false,
  "deletions": false
}
```
