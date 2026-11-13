#!/usr/bin/env bash
# Removes billing account IDs from Terraform output (stdin to stdout) before it goes anywhere
# GitHub does not mask secrets: PR comments posted through the API and job summaries. The budget's
# name (billingAccounts/<account>/budgets/<id>) shows up in in-place diffs and in errors.
#
# Redacts the value of TF_VAR_billing_account_id or BILLING_ACCOUNT_ID when set, anything after
# "billingAccounts/", and any string shaped like a billing account ID (XXXXXX-XXXXXX-XXXXXX).
set -euo pipefail

text=$(
  cat
  echo x
) # the x keeps trailing newlines through the command substitution
text=${text%x}

for value in "${TF_VAR_billing_account_id:-}" "${BILLING_ACCOUNT_ID:-}"; do
  if [[ -n $value ]]; then
    text=${text//"$value"/***}
  fi
done

printf '%s' "$text" | sed -E \
  -e 's#billingAccounts/[^/[:space:]"]+#billingAccounts/***#g' \
  -e 's/(^|[^0-9A-Za-z-])[0-9A-Fa-f]{6}-[0-9A-Fa-f]{6}-[0-9A-Fa-f]{6}([^0-9A-Za-z-]|$)/\1***\2/g'
