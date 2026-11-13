# Reduces `terraform show -json <plan>` to terraform-drift.json for tools/evidence.
#
#   terraform show -json drift.tfplan | jq --argjson code "$exit_code" -f tools/evidence/drift.jq
#
# Plan JSON holds every attribute value, sensitive ones included. This keeps only resource
# addresses, actions and attribute names; values are compared here and never printed.
(.relevant_attributes // []) as $relevant
| {
    exitCode: $code,
    changes: [
      .resource_changes[]?
      | select(.change.actions != ["no-op"] and .change.actions != ["read"])
      | {address, actions: .change.actions}
    ],
    drift: [
      .resource_drift[]?
      | .address as $address
      | (if (.change.before | type) == "object" then .change.before else {} end) as $before
      | (if (.change.after | type) == "object" then .change.after else {} end) as $after
      # Top-level attributes whose refreshed value differs from state.
      | ([($before | keys[]), ($after | keys[])] | unique | map(select($before[.] != $after[.]))) as $changed
      # Attributes of this resource that the plan depends on; an empty path means all of them.
      | ([$relevant[] | select(.resource == $address) | (.attribute[0] // "*") | tostring] | unique) as $used
      | {
          address: $address,
          actions: .change.actions,
          attributes: $changed,
          relevantAttributes: (if ($used | index("*")) then $changed else [$changed[] | select(. as $a | $used | index($a))] end)
        }
    ]
  }
