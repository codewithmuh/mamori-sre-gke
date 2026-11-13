"""Run conditions: what the agent ran with, checked against what the operator typed.

The switches that change what an investigation sees (postmortems, fault labels, the chaos
tool, the model, the budget) live in the agent's Deployment, in Git (Argo CD reverts a
`kubectl set env` within seconds). A session records them from there, refuses to start when
a typed `--label` disagrees, and each investigation's own `agentConfig` is checked again when
the run is scored.
"""

from __future__ import annotations

from collections.abc import Mapping

from .jsonutil import DocumentError
from .scoring import CONFIG_KEYS, same_setting


def session_conditions(
    typed: Mapping[str, str],
    deployment_env: Mapping[str, str],
    live_config: Mapping[str, object] | None = None,
) -> dict[str, str]:
    """The session's labels: the typed ones plus every evaluation switch the Deployment sets.

    Raises DocumentError when a typed AGENT_* label disagrees with the Deployment, names a
    switch the Deployment doesn't set (the code default can't be checked), or when the running
    agent reports other settings than its Deployment (not synced yet).
    """
    problems = []
    labels = dict(typed)
    for key in CONFIG_KEYS:
        if key in deployment_env:
            if key in typed and not same_setting(typed[key], deployment_env[key]):
                problems.append(
                    f"{key}: you typed {typed[key]}, the agent Deployment has {deployment_env[key]}"
                )
            labels[key] = deployment_env[key]
    for key in typed:
        if key.startswith("AGENT_") and key not in deployment_env:
            problems.append(f"{key}: not set on the agent Deployment, so it can't be checked")
    for key, config_key in CONFIG_KEYS.items():
        if (
            live_config
            and key in labels
            and config_key in live_config
            and not same_setting(labels[key], live_config[config_key])
        ):
            problems.append(
                f"{key}: the Deployment says {labels[key]}, the running agent reports "
                f"{live_config[config_key]} (not rolled out yet?)"
            )
    if problems:
        raise DocumentError("run conditions don't match the agent:\n  " + "\n  ".join(problems))
    return labels
