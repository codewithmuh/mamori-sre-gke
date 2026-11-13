"""Offline scoring: a recording (live session or fixture) + the fault library -> results.

Live runs go through the same function, so a scoring fix can be re-applied to an old session
with `mamori-evals replay` without touching the cluster.
"""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from datetime import datetime

from .aggregate import summarize
from .baseline import Baseline, PostmortemIndex, prior_postmortem, summarize_baseline
from .faults import Fault
from .jsonutil import DocumentError, JSONDict
from .records import Recording
from .report import build_results
from .scoring import RunScore, score_run

# A run counts as injected into a clean window when every journey's 1-hour burn rate was
# below this: the hour before was within the error budget's pace.
DEFAULT_CLEAN_BURN = 1.0


def merge_recordings(recordings: Sequence[Recording]) -> Recording:
    """Several sessions as one dataset (20-30 faults take more than one session).

    Refuses to mix synthetic fixtures with lab sessions, or sessions run under different
    conditions (labels), since one headline number would then describe neither.
    """
    if not recordings:
        raise DocumentError("no recordings")
    if len(recordings) == 1:
        return recordings[0]
    if len({recording.synthetic for recording in recordings}) > 1:
        raise DocumentError("never mix synthetic fixtures with lab sessions")
    labels = [recording.metadata.get("labels") or {} for recording in recordings]
    if any(label != labels[0] for label in labels):
        raise DocumentError(
            "the sessions ran under different conditions (labels differ); replay them apart"
        )
    starts = [str(r.metadata["startedAt"]) for r in recordings if r.metadata.get("startedAt")]
    ends = [str(r.metadata["finishedAt"]) for r in recordings if r.metadata.get("finishedAt")]
    metadata: dict[str, object] = {
        "synthetic": recordings[0].synthetic,
        "labels": labels[0],
        "sessions": [recording.metadata.get("session") for recording in recordings],
    }
    # RFC3339 UTC strings with the same format sort in time order.
    if starts:
        metadata["startedAt"] = min(starts)
    if ends:
        metadata["finishedAt"] = max(ends)
    return Recording(metadata, [run for recording in recordings for run in recording.runs])


def score_recording(
    recording: Recording,
    faults: Mapping[str, Fault],
    postmortems: PostmortemIndex | None,
    clean_burn: float = DEFAULT_CLEAN_BURN,
) -> list[RunScore]:
    unknown = sorted({run.fault_id for run in recording.runs} - set(faults))
    if unknown:
        raise DocumentError(f"the recording has runs of faults not in the library: {unknown}")
    labels = recording.metadata.get("labels")
    session_labels: Mapping[str, object] = labels if isinstance(labels, Mapping) else {}
    scores = []
    for run in recording.runs:
        # What the agent actually ran with, else what the session said it would.
        setting = (
            run.investigation.agent_config.get("postmortemsGamedays")
            if run.investigation is not None
            else None
        ) or session_labels.get("AGENT_POSTMORTEMS_GAMEDAYS")
        prior = prior_postmortem(postmortems, run.fault_id, run.injected_at, setting)
        scores.append(score_run(run, faults[run.fault_id], prior, clean_burn, session_labels))
    return scores


def results_for(
    recording: Recording,
    faults: Mapping[str, Fault],
    postmortems: PostmortemIndex | None,
    baselines: Sequence[Baseline],
    mode: str,
    generated_at: datetime,
    extra_metadata: Mapping[str, object] | None = None,
    clean_burn: float = DEFAULT_CLEAN_BURN,
) -> JSONDict:
    scores = score_recording(recording, faults, postmortems, clean_burn)
    metadata: dict[str, object] = {
        **recording.metadata,
        **(extra_metadata or {}),
        "mode": mode,
        "synthetic": recording.synthetic,
        "cleanBurn1h": clean_burn,
    }
    return build_results(
        scores,
        summarize(scores),
        metadata,
        [summarize_baseline(baseline, faults) for baseline in baselines],
        generated_at,
    )
