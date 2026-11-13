"""Read tools on the Kubernetes API: pods, events, deployments, rollout history, pod logs, nodes,
Argo CD Applications, Istio config and Chaos Mesh experiments.

The client has a single verb, GET. In the cluster it uses the pod's service account token, which
RBAC limits to get/list/watch on named resources, never Secrets (deploy/agent/rbac.yaml). For a
local run, point AGENT_K8S_API_URL at `kubectl proxy` (http://127.0.0.1:8001).

Objects are printed as `<kind>/<name>`, the target format the executor expects.
"""

from __future__ import annotations

import json
import os
from datetime import UTC, datetime, timedelta
from typing import Any

from mamori_agent.http import Http, HttpError, checked_json
from mamori_agent.safety import strip_fault_labels

ISTIO_KINDS: tuple[tuple[str, str, str], ...] = (
    # (API group/version path, plural, kind for targets)
    ("networking.istio.io/v1", "virtualservices", "virtualservice"),
    ("networking.istio.io/v1", "destinationrules", "destinationrule"),
    ("networking.istio.io/v1", "sidecars", "sidecar"),
    ("security.istio.io/v1", "authorizationpolicies", "authorizationpolicy"),
)
# The kinds the game-day faults use (#23). Others are not readable (RBAC lists these only).
CHAOS_KINDS: tuple[str, ...] = ("podchaos", "networkchaos", "stresschaos")
ARGOCD_NAMESPACE = "argocd"
_SPEC_CHARS = 900


class KubeClient:
    def __init__(self, http: Http, base_url: str, token_file: str, timeout: float) -> None:
        self._http = http
        self._base = base_url.rstrip("/")
        self._token_file = token_file
        self._timeout = timeout

    def _headers(self) -> dict[str, str]:
        # Read per request: the kubelet rotates projected tokens.
        if self._token_file and os.path.exists(self._token_file):
            with open(self._token_file, encoding="utf-8") as fh:
                return {"Authorization": f"Bearer {fh.read().strip()}"}
        return {}

    def get(self, path: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
        response = self._http.get(
            f"{self._base}{path}", params=params, headers=self._headers(), timeout=self._timeout
        )
        data = checked_json(response, f"GET {path}")
        return data if isinstance(data, dict) else {}

    def get_text(self, path: str, params: dict[str, Any] | None = None) -> str:
        response = self._http.get(
            f"{self._base}{path}", params=params, headers=self._headers(), timeout=self._timeout
        )
        if response.status >= 400:
            raise HttpError(response.status, f"GET {path}: {response.text[:300]}")
        return response.text


def _parse_time(value: Any) -> datetime | None:
    if not isinstance(value, str) or not value:
        return None
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None


def _age(value: Any, now: datetime | None = None) -> str:
    moment = _parse_time(value)
    if moment is None:
        return "?"
    seconds = int(((now or datetime.now(UTC)) - moment).total_seconds())
    if seconds < 120:
        return f"{seconds}s"
    if seconds < 7200:
        return f"{seconds // 60}m"
    if seconds < 172_800:
        return f"{seconds // 3600}h"
    return f"{seconds // 86400}d"


def _labels(meta: dict[str, Any]) -> str:
    labels = meta.get("labels") or {}
    shown = {k: v for k, v in labels.items() if not k.startswith(("pod-template-hash",))}
    return ",".join(f"{k}={v}" for k, v in sorted(shown.items()))


def _compact(value: Any, limit: int = _SPEC_CHARS) -> str:
    text = json.dumps(value, sort_keys=True, separators=(",", ":"))
    return text if len(text) <= limit else text[:limit] + "...(cut)"


def _containers_summary(template_spec: dict[str, Any]) -> list[str]:
    out = []
    for c in template_spec.get("containers") or []:
        env = []
        for e in c.get("env") or []:
            if "value" in e:
                env.append(f"{e.get('name')}={str(e['value'])[:120]}")
            elif "valueFrom" in e:
                source = next(iter(e["valueFrom"]), "ref")
                env.append(f"{e.get('name')}=<from {source}>")
        resources = c.get("resources") or {}
        line = f"{c.get('name')}: image={c.get('image')}"
        if c.get("args"):
            line += f" args={_compact(c['args'], 200)}"
        if resources:
            line += f" resources={_compact(resources, 200)}"
        if env:
            line += " env: " + " ".join(env)
        out.append(line)
    return out


class KubeReads:
    def __init__(
        self, client: KubeClient, log_namespaces: list[str], hide_fault_labels: bool
    ) -> None:
        self._k = client
        self._log_namespaces = log_namespaces
        self._hide = hide_fault_labels

    def _items(self, path: str, params: dict[str, Any] | None = None) -> list[dict[str, Any]]:
        items = self._k.get(path, params).get("items") or []
        return [strip_fault_labels(i) if self._hide else i for i in items]

    def _one(self, path: str) -> dict[str, Any]:
        obj = self._k.get(path)
        return strip_fault_labels(obj) if self._hide else obj

    # --- pods ----------------------------------------------------------------------------------

    def pods(self, namespace: str, label_selector: str) -> str:
        params = {"labelSelector": label_selector} if label_selector else None
        pods = self._items(f"/api/v1/namespaces/{namespace}/pods", params)
        if not pods:
            return f"no pods in {namespace}" + (
                f" matching {label_selector}" if label_selector else ""
            )
        lines = ["pod | ready | phase | restarts | age | node | problems"]
        for pod in sorted(pods, key=lambda p: p["metadata"]["name"]):
            meta, status = pod["metadata"], pod.get("status") or {}
            statuses = status.get("containerStatuses") or []
            ready = sum(1 for s in statuses if s.get("ready"))
            restarts = sum(int(s.get("restartCount", 0)) for s in statuses)
            problems = []
            for s in statuses:
                waiting = (s.get("state") or {}).get("waiting")
                if waiting:
                    problems.append(f"{s['name']} waiting {waiting.get('reason')}")
                last = (s.get("lastState") or {}).get("terminated")
                if last:
                    problems.append(
                        f"{s['name']} last terminated {last.get('reason')} "
                        f"exit={last.get('exitCode')} {_age(last.get('finishedAt'))} ago"
                    )
            for cond in status.get("conditions") or []:
                if cond.get("status") != "True" and cond.get("type") in ("Ready", "PodScheduled"):
                    problems.append(f"{cond['type']}=False {cond.get('reason', '')}".strip())
            node = (pod.get("spec") or {}).get("nodeName", "-")
            lines.append(
                f"pod/{meta['name']} | {ready}/{len(statuses)} | {status.get('phase')} | "
                f"{restarts} | {_age(meta.get('creationTimestamp'))} | {node} | "
                + ("; ".join(problems) or "-")
            )
        return "\n".join(lines)

    # --- events --------------------------------------------------------------------------------

    def events(
        self, namespace: str, object_name: str, minutes: int, warnings_only: bool, limit: int
    ) -> str:
        params: dict[str, Any] = {}
        selectors = []
        if object_name:
            selectors.append(f"involvedObject.name={object_name}")
        if warnings_only:
            selectors.append("type=Warning")
        if selectors:
            params["fieldSelector"] = ",".join(selectors)
        items = self._items(f"/api/v1/namespaces/{namespace}/events", params or None)
        since = datetime.now(UTC) - timedelta(minutes=minutes)

        def when(e: dict[str, Any]) -> datetime:
            return (
                _parse_time(e.get("lastTimestamp"))
                or _parse_time(e.get("eventTime"))
                or _parse_time((e.get("metadata") or {}).get("creationTimestamp"))
                or datetime.min.replace(tzinfo=UTC)
            )

        recent = sorted((e for e in items if when(e) >= since), key=when, reverse=True)
        if not recent:
            return f"no events in {namespace} in the last {minutes} min"
        lines = []
        for e in recent[:limit]:
            obj = e.get("involvedObject") or {}
            lines.append(
                f"{_age(when(e).isoformat())} ago {e.get('type')} {e.get('reason')} "
                f"{str(obj.get('kind', '')).lower()}/{obj.get('name')} x{e.get('count') or 1}: "
                f"{str(e.get('message', '')).replace(chr(10), ' ')[:300]}"
            )
        if len(recent) > limit:
            lines.append(f"... {len(recent) - limit} older events")
        return "\n".join(lines)

    # --- deployments and rollout history -------------------------------------------------------

    def deployments(self, namespace: str, name: str) -> str:
        if name:
            deps = [self._one(f"/apis/apps/v1/namespaces/{namespace}/deployments/{name}")]
        else:
            deps = self._items(f"/apis/apps/v1/namespaces/{namespace}/deployments")
        if not deps:
            return f"no deployments in {namespace}"
        lines = []
        for d in sorted(deps, key=lambda x: x["metadata"]["name"]):
            meta, spec, status = d["metadata"], d.get("spec") or {}, d.get("status") or {}
            revision = (meta.get("annotations") or {}).get("deployment.kubernetes.io/revision", "?")
            line = (
                f"deployment/{meta['name']} desired={spec.get('replicas', 1)} "
                f"ready={status.get('readyReplicas', 0)} "
                f"updated={status.get('updatedReplicas', 0)} "
                f"available={status.get('availableReplicas', 0)} revision={revision} "
                f"age={_age(meta.get('creationTimestamp'))}"
            )
            if status.get("observedGeneration") != meta.get("generation"):
                line += " (rollout not yet observed)"
            bad = [
                f"{c.get('type')}={c.get('status')} {c.get('reason')}: {c.get('message', '')[:160]}"
                for c in status.get("conditions") or []
                if c.get("status") != "True"
            ]
            if bad:
                line += " | " + "; ".join(bad)
            lines.append(line)
            if name:
                lines.append(f"  labels: {_labels(meta) or '-'}")
                template = (spec.get("template") or {}).get("spec") or {}
                lines.extend(f"  {c}" for c in _containers_summary(template))
            else:
                images = [
                    c.get("image", "")
                    for c in ((spec.get("template") or {}).get("spec") or {}).get("containers")
                    or []
                ]
                lines.append(f"  images: {', '.join(i.rsplit('/', 1)[-1] for i in images)}")
        return "\n".join(lines)

    def rollout_history(self, namespace: str, deployment: str) -> str:
        sets = [
            rs
            for rs in self._items(f"/apis/apps/v1/namespaces/{namespace}/replicasets")
            if any(
                o.get("kind") == "Deployment" and o.get("name") == deployment
                for o in (rs["metadata"].get("ownerReferences") or [])
            )
        ]
        if not sets:
            return f"no ReplicaSets owned by deployment/{deployment} in {namespace}"

        def revision(rs: dict[str, Any]) -> int:
            value = (rs["metadata"].get("annotations") or {}).get(
                "deployment.kubernetes.io/revision", "0"
            )
            return int(value) if str(value).isdigit() else 0

        sets.sort(key=revision, reverse=True)
        summaries = [
            (
                rs,
                _containers_summary(
                    ((rs.get("spec") or {}).get("template") or {}).get("spec") or {}
                ),
            )
            for rs in sets[:6]
        ]
        lines = [f"rollout history of deployment/{deployment} (newest first)"]
        for index, (rs, summary) in enumerate(summaries):
            meta, spec, status = rs["metadata"], rs.get("spec") or {}, rs.get("status") or {}
            cause = (meta.get("annotations") or {}).get("kubernetes.io/change-cause", "")
            lines.append(
                f"revision {revision(rs)}: replicaset/{meta['name']} "
                f"created {_age(meta.get('creationTimestamp'))} ago, "
                f"replicas {status.get('readyReplicas', 0)}/{spec.get('replicas', 0)}"
                + (f", change-cause: {cause}" if cause else "")
            )
            older = summaries[index + 1][1] if index + 1 < len(summaries) else None
            if older is None:
                lines.extend(f"  {c}" for c in summary)
            elif summary != older:
                lines.append("  changed from the revision below:")
                lines.extend(f"  - {old}" for old in older if old not in summary)
                lines.extend(f"  + {new}" for new in summary if new not in older)
            else:
                lines.append("  (same containers as the revision below)")
        return "\n".join(lines)

    # --- pod logs ------------------------------------------------------------------------------

    def pod_logs(
        self, namespace: str, pod: str, container: str, tail_lines: int, previous: bool
    ) -> str:
        if namespace not in self._log_namespaces:
            return (
                f"refused: pod logs are readable only in {', '.join(self._log_namespaces)} "
                "(use logs_search for Cloud Logging instead)"
            )
        name = pod.removeprefix("pod/")
        names = [p["metadata"]["name"] for p in self._items(f"/api/v1/namespaces/{namespace}/pods")]
        if name not in names:
            # A deployment name: take its first pod, so the model needn't list pods first.
            matches = sorted(n for n in names if n.startswith(f"{name}-"))
            if not matches:
                return f"no pod {name} (or pods of deployment {name}) in {namespace}"
            name = matches[0]
        params: dict[str, Any] = {"tailLines": tail_lines, "limitBytes": 65536}
        if container:
            params["container"] = container
        if previous:
            params["previous"] = "true"
        text = self._k.get_text(f"/api/v1/namespaces/{namespace}/pods/{name}/log", params)
        header = f"logs of pod/{name}" + (f" container {container}" if container else "")
        which = ", previous container" if previous else ""
        return f"{header} (last {tail_lines} lines{which}):\n{text or '(empty)'}"

    # --- nodes ---------------------------------------------------------------------------------

    def nodes(self) -> str:
        nodes = self._items("/api/v1/nodes")
        if not nodes:
            return "no nodes (the node pool may be scaled to zero by the spend guard)"
        lines = []
        for n in sorted(nodes, key=lambda x: x["metadata"]["name"]):
            meta, spec, status = n["metadata"], n.get("spec") or {}, n.get("status") or {}
            labels = meta.get("labels") or {}
            conds = {c.get("type"): c for c in status.get("conditions") or []}
            ready = conds.get("Ready", {})
            problems = [
                f"{t}={c.get('status')}"
                for t, c in conds.items()
                if t != "Ready" and c.get("status") == "True"
            ]
            alloc = status.get("allocatable") or {}
            line = (
                f"node/{meta['name']} Ready={ready.get('status', '?')} "
                f"(since {_age(ready.get('lastTransitionTime'))}) "
                f"age={_age(meta.get('creationTimestamp'))} "
                f"spot={labels.get('cloud.google.com/gke-spot', 'false')} "
                f"cpu={alloc.get('cpu', '?')} mem={alloc.get('memory', '?')}"
            )
            if spec.get("unschedulable"):
                line += " UNSCHEDULABLE"
            if spec.get("taints"):
                line += " taints=" + ",".join(
                    f"{t.get('key')}:{t.get('effect')}" for t in spec["taints"]
                )
            if problems:
                line += " problems: " + ", ".join(problems)
            lines.append(line)
        return "\n".join(lines)

    # --- Argo CD -------------------------------------------------------------------------------

    def argocd_applications(self, name: str) -> str:
        base = f"/apis/argoproj.io/v1alpha1/namespaces/{ARGOCD_NAMESPACE}/applications"
        apps = [self._one(f"{base}/{name}")] if name else self._items(base)
        if not apps:
            return "no Argo CD Applications"
        lines = []
        for app in sorted(apps, key=lambda a: a["metadata"]["name"]):
            meta, spec, status = app["metadata"], app.get("spec") or {}, app.get("status") or {}
            sync, health = status.get("sync") or {}, status.get("health") or {}
            op = status.get("operationState") or {}
            source = spec.get("source") or (spec.get("sources") or [{}])[0]
            line = (
                f"application/{meta['name']} sync={sync.get('status')} "
                f"health={health.get('status')} revision={str(sync.get('revision', ''))[:10]} "
                f"path={source.get('path') or source.get('chart')} "
                f"last-op={op.get('phase', '-')} {_age(op.get('finishedAt'))} ago"
            )
            if op.get("message") and op.get("phase") != "Succeeded":
                line += f" ({str(op['message'])[:200]})"
            lines.append(line)
            history = (status.get("history") or [])[-3:]
            for h in reversed(history):
                lines.append(
                    f"  deployed {str(h.get('revision', ''))[:10]} {_age(h.get('deployedAt'))} ago"
                )
            if name:
                for cond in status.get("conditions") or []:
                    lines.append(
                        f"  condition {cond.get('type')}: {str(cond.get('message', ''))[:200]}"
                    )
                for res in status.get("resources") or []:
                    res_health = (res.get("health") or {}).get("status", "")
                    if res.get("status") != "Synced" or res_health not in ("", "Healthy"):
                        lines.append(
                            f"  {str(res.get('kind', '')).lower()}/{res.get('name')} "
                            f"sync={res.get('status')} health={res_health or '-'}"
                        )
        return "\n".join(lines)

    # --- Istio and Chaos Mesh ------------------------------------------------------------------

    def mesh_config(self, namespace: str) -> str:
        lines = []
        for group_version, plural, kind in ISTIO_KINDS:
            try:
                items = self._items(f"/apis/{group_version}/namespaces/{namespace}/{plural}")
            except HttpError as exc:
                lines.append(f"{plural}: not readable ({exc.status})")
                continue
            for obj in sorted(items, key=lambda o: o["metadata"]["name"]):
                meta = obj["metadata"]
                lines.append(
                    f"{kind}/{meta['name']} age={_age(meta.get('creationTimestamp'))} "
                    f"labels={_labels(meta) or '-'} spec={_compact(obj.get('spec') or {})}"
                )
        return "\n".join(lines) or f"no Istio objects in {namespace}"

    def chaos_experiments(self, namespace: str) -> str:
        lines = []
        for plural in CHAOS_KINDS:
            try:
                items = self._items(
                    f"/apis/chaos-mesh.org/v1alpha1/namespaces/{namespace}/{plural}"
                )
            except HttpError as exc:
                if exc.status == 404:
                    lines.append(f"{plural}: Chaos Mesh not installed")
                    continue
                raise
            for obj in sorted(items, key=lambda o: o["metadata"]["name"]):
                meta, status = obj["metadata"], obj.get("status") or {}
                phase = (status.get("experiment") or {}).get("desiredPhase", "?")
                conditions = ",".join(
                    f"{c.get('type')}={c.get('status')}" for c in status.get("conditions") or []
                )
                lines.append(
                    f"{plural}/{meta['name']} age={_age(meta.get('creationTimestamp'))} "
                    f"phase={phase} {conditions} labels={_labels(meta) or '-'} "
                    f"spec={_compact(obj.get('spec') or {})}"
                )
        return "\n".join(lines) or f"no chaos experiments in {namespace}"
