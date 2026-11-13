from __future__ import annotations

from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest

from conftest import FakeHttp, ok, text
from mamori_agent.http import HttpError
from mamori_agent.tools.kube import KubeClient, KubeReads, _age

API = "https://kubernetes.default.svc"


def ago(**delta: int) -> str:
    return (datetime.now(UTC) - timedelta(**delta)).isoformat().replace("+00:00", "Z")


def reads(routes: dict, hide: bool = False, token_file: str = "") -> tuple[KubeReads, FakeHttp]:
    http = FakeHttp(routes)
    return KubeReads(
        KubeClient(http, API, token_file, 5), ["boutique", "istio-ingress"], hide
    ), http


def test_client_sends_rotating_token(tmp_path: Path) -> None:
    token = tmp_path / "token"
    token.write_text("abc\n")
    k, http = reads({"/api/v1/nodes": ok({"items": []})}, token_file=str(token))
    k.nodes()
    assert http.calls[0]["headers"] == {"Authorization": "Bearer abc"}
    token.write_text("def")
    k.nodes()
    assert http.calls[1]["headers"] == {"Authorization": "Bearer def"}


def test_client_only_gets() -> None:
    client = KubeClient(FakeHttp({"/x": text("[]")}), API, "", 5)
    assert client.get("/x") == {}
    with pytest.raises(HttpError, match="403"):
        KubeClient(FakeHttp({"/x": text("forbidden", 403)}), API, "", 5).get_text("/x")
    assert not hasattr(client, "post") and not hasattr(client, "delete")


def test_age() -> None:
    assert _age(None) == "?"
    assert _age("bad") == "?"
    assert _age(ago(seconds=30)).endswith("s")
    assert _age(ago(minutes=30)) == "30m"
    assert _age(ago(hours=5)) == "5h"
    assert _age(ago(days=3)) == "3d"


def test_pods_show_problems() -> None:
    pod = {
        "metadata": {"name": "currencyservice-abc", "creationTimestamp": ago(minutes=5)},
        "spec": {"nodeName": "node-1"},
        "status": {
            "phase": "Running",
            "containerStatuses": [
                {
                    "name": "server",
                    "ready": False,
                    "restartCount": 3,
                    "state": {"waiting": {"reason": "CrashLoopBackOff"}},
                    "lastState": {
                        "terminated": {
                            "reason": "OOMKilled",
                            "exitCode": 137,
                            "finishedAt": ago(seconds=40),
                        }
                    },
                },
                {"name": "istio-proxy", "ready": True, "restartCount": 0},
            ],
            "conditions": [{"type": "Ready", "status": "False", "reason": "ContainersNotReady"}],
        },
    }
    k, http = reads({"/pods": ok({"items": [pod]})})
    out = k.pods("boutique", "app=currencyservice")
    assert http.calls[0]["params"] == {"labelSelector": "app=currencyservice"}
    assert "pod/currencyservice-abc | 1/2 | Running | 3 | 5m | node-1 |" in out
    assert "server waiting CrashLoopBackOff" in out
    assert "last terminated OOMKilled exit=137" in out
    assert "Ready=False ContainersNotReady" in out
    empty, _ = reads({"/pods": ok({"items": []})})
    assert empty.pods("boutique", "app=x") == "no pods in boutique matching app=x"


def test_events_filters_and_sorts() -> None:
    items = [
        {
            "type": "Warning",
            "reason": "BackOff",
            "involvedObject": {"kind": "Pod", "name": "a"},
            "message": "back-off",
            "lastTimestamp": ago(minutes=2),
            "count": 5,
        },
        {
            "type": "Normal",
            "reason": "Pulled",
            "involvedObject": {"kind": "Pod", "name": "b"},
            "message": "old",
            "lastTimestamp": ago(hours=5),
        },
        {
            "type": "Normal",
            "reason": "Killing",
            "involvedObject": {"kind": "Pod", "name": "c"},
            "message": "m",
            "eventTime": ago(minutes=1),
        },
        {
            "type": "Normal",
            "reason": "Created",
            "involvedObject": {"kind": "Pod", "name": "d"},
            "message": "n",
            "metadata": {"creationTimestamp": ago(minutes=3)},
        },
    ]
    k, http = reads({"/events": ok({"items": items})})
    out = k.events("boutique", "a", 60, True, 2)
    assert http.calls[0]["params"] == {"fieldSelector": "involvedObject.name=a,type=Warning"}
    lines = out.splitlines()
    assert "Killing pod/c" in lines[0]
    assert "BackOff pod/a x5: back-off" in lines[1]
    assert lines[2] == "... 1 older events"
    none, _ = reads({"/events": ok({"items": []})})
    assert none.events("boutique", "", 10, False, 5) == "no events in boutique in the last 10 min"


def _deployment(name: str, image: str, env: list, generation: int = 2) -> dict:
    return {
        "metadata": {
            "name": name,
            "generation": generation,
            "creationTimestamp": ago(days=2),
            "annotations": {"deployment.kubernetes.io/revision": "4"},
            "labels": {"app": name, "mamori.dev/fault": "true"},
        },
        "spec": {
            "replicas": 2,
            "template": {
                "spec": {
                    "containers": [
                        {
                            "name": "server",
                            "image": image,
                            "env": env,
                            "args": ["--x"],
                            "resources": {"requests": {"cpu": "100m"}},
                        }
                    ]
                }
            },
        },
        "status": {
            "observedGeneration": 1,
            "readyReplicas": 1,
            "updatedReplicas": 1,
            "availableReplicas": 1,
            "conditions": [
                {
                    "type": "Available",
                    "status": "False",
                    "reason": "MinimumReplicasUnavailable",
                    "message": "no",
                }
            ],
        },
    }


def test_deployments_list_and_detail_with_hidden_labels() -> None:
    dep = _deployment(
        "checkoutservice",
        "repo/checkoutservice:v1",
        [{"name": "PORT", "value": "5050"}, {"name": "S", "valueFrom": {"secretKeyRef": {}}}],
    )
    k, _ = reads(
        {"/deployments/checkoutservice": ok(dep), "/deployments": ok({"items": [dep]})}, hide=True
    )
    listing = k.deployments("boutique", "")
    assert "deployment/checkoutservice desired=2 ready=1" in listing
    assert "(rollout not yet observed)" in listing
    assert "Available=False MinimumReplicasUnavailable" in listing
    assert "images: checkoutservice:v1" in listing
    detail = k.deployments("boutique", "checkoutservice")
    assert "labels: app=checkoutservice" in detail and "mamori.dev" not in detail
    assert "env: PORT=5050 S=<from secretKeyRef>" in detail
    assert 'args=["--x"]' in detail
    empty, _ = reads({"/deployments": ok({"items": []})})
    assert empty.deployments("boutique", "") == "no deployments in boutique"


def test_rollout_history_shows_what_changed() -> None:
    def rs(rev: str, image: str, env_value: str) -> dict:
        return {
            "metadata": {
                "name": f"checkoutservice-{rev}",
                "creationTimestamp": ago(hours=int(rev)),
                "annotations": {"deployment.kubernetes.io/revision": rev},
                "ownerReferences": [{"kind": "Deployment", "name": "checkoutservice"}],
            },
            "spec": {
                "replicas": 1,
                "template": {
                    "spec": {
                        "containers": [
                            {
                                "name": "server",
                                "image": image,
                                "env": [{"name": "ADDR", "value": env_value}],
                            }
                        ]
                    }
                },
            },
            "status": {"readyReplicas": 1},
        }

    other = rs("9", "x", "y")
    other["metadata"]["ownerReferences"] = [{"kind": "Deployment", "name": "frontend"}]
    k, _ = reads(
        {
            "/replicasets": ok(
                {
                    "items": [
                        rs("1", "img:v1", "good:1"),
                        rs("3", "img:v1", "bad:1"),
                        rs("2", "img:v1", "good:1"),
                        other,
                    ]
                }
            )
        }
    )
    out = k.rollout_history("boutique", "checkoutservice")
    lines = out.splitlines()
    assert lines[1].startswith("revision 3: replicaset/checkoutservice-3")
    assert "  - server: image=img:v1 env: ADDR=good:1" in out
    assert "  + server: image=img:v1 env: ADDR=bad:1" in out
    assert "(same containers as the revision below)" in out
    assert lines[-1] == "  server: image=img:v1 env: ADDR=good:1"
    none, _ = reads({"/replicasets": ok({"items": []})})
    assert none.rollout_history("boutique", "x").startswith("no ReplicaSets")


def test_pod_logs_namespace_allow_list_and_deployment_names() -> None:
    pods = {
        "items": [
            {"metadata": {"name": "currencyservice-7d9-abc"}},
            {"metadata": {"name": "frontend-1"}},
        ]
    }
    k, http = reads({"/log": text("line1\nline2"), "/pods": ok(pods)})
    assert k.pod_logs("argocd", "x", "", 10, False).startswith("refused")
    out = k.pod_logs("boutique", "currencyservice", "server", 50, True)
    assert out.startswith(
        "logs of pod/currencyservice-7d9-abc container server (last 50 lines, previous container)"
    )
    log_call = http.calls[-1]
    assert log_call["params"] == {
        "tailLines": 50,
        "limitBytes": 65536,
        "container": "server",
        "previous": "true",
    }
    assert k.pod_logs("boutique", "pod/frontend-1", "", 5, False).endswith("line1\nline2")
    assert k.pod_logs("boutique", "nothere", "", 5, False).startswith("no pod nothere")


def test_nodes() -> None:
    node = {
        "metadata": {
            "name": "gke-1",
            "creationTimestamp": ago(minutes=3),
            "labels": {"cloud.google.com/gke-spot": "true"},
        },
        "spec": {
            "unschedulable": True,
            "taints": [{"key": "node.kubernetes.io/not-ready", "effect": "NoSchedule"}],
        },
        "status": {
            "allocatable": {"cpu": "1930m", "memory": "6Gi"},
            "conditions": [
                {"type": "Ready", "status": "False", "lastTransitionTime": ago(minutes=2)},
                {"type": "MemoryPressure", "status": "True"},
            ],
        },
    }
    k, _ = reads({"/api/v1/nodes": ok({"items": [node]})})
    out = k.nodes()
    assert "node/gke-1 Ready=False" in out and "spot=true" in out
    assert "UNSCHEDULABLE" in out and "not-ready:NoSchedule" in out and "MemoryPressure=True" in out
    empty, _ = reads({"/api/v1/nodes": ok({"items": []})})
    assert "spend guard" in empty.nodes()


def test_argocd_applications() -> None:
    app = {
        "metadata": {"name": "online-boutique"},
        "spec": {"source": {"path": "deploy/online-boutique"}},
        "status": {
            "sync": {"status": "OutOfSync", "revision": "0123456789abcdef"},
            "health": {"status": "Degraded"},
            "operationState": {
                "phase": "Failed",
                "finishedAt": ago(minutes=4),
                "message": "hook failed",
            },
            "history": [
                {"revision": "aaaaaaaaaaaa", "deployedAt": ago(days=1)},
                {"revision": "bbbbbbbbbbbb", "deployedAt": ago(minutes=10)},
            ],
            "conditions": [{"type": "SyncError", "message": "boom"}],
            "resources": [
                {
                    "kind": "Deployment",
                    "name": "checkoutservice",
                    "status": "Synced",
                    "health": {"status": "Degraded"},
                },
                {
                    "kind": "Service",
                    "name": "ok",
                    "status": "Synced",
                    "health": {"status": "Healthy"},
                },
            ],
        },
    }
    k, http = reads(
        {"/applications/online-boutique": ok(app), "/applications": ok({"items": [app]})}
    )
    out = k.argocd_applications("")
    assert "application/online-boutique sync=OutOfSync health=Degraded revision=0123456789" in out
    assert "(hook failed)" in out
    assert out.splitlines()[1].startswith("  deployed bbbbbbbbbb 10m ago")
    detail = k.argocd_applications("online-boutique")
    assert "condition SyncError: boom" in detail
    assert "deployment/checkoutservice sync=Synced health=Degraded" in detail
    assert "service/ok" not in detail
    assert "/namespaces/argocd/applications" in http.calls[0]["url"]


def test_mesh_config_lists_istio_kinds_and_tolerates_missing_access() -> None:
    vs = {
        "metadata": {"name": "f01-currency-abort", "labels": {"mamori.dev/fault": "true"}},
        "spec": {
            "hosts": ["currencyservice"],
            "http": [{"fault": {"abort": {"percentage": {"value": 50}}}}],
        },
    }
    k, _ = reads(
        {
            "/virtualservices": ok({"items": [vs]}),
            "/destinationrules": ok({"items": []}),
            "/sidecars": text("forbidden", 403),
            "/authorizationpolicies": ok({"items": []}),
        }
    )
    out = k.mesh_config("boutique")
    assert "virtualservice/f01-currency-abort" in out and '"abort"' in out
    assert "mamori.dev/fault=true" in out
    assert "sidecars: not readable (403)" in out
    hidden, _ = reads({"/virtualservices": ok({"items": [vs]}), "/": ok({"items": []})}, hide=True)
    assert "mamori.dev" not in hidden.mesh_config("boutique")
    empty, _ = reads({"/": ok({"items": []})})
    assert empty.mesh_config("boutique") == "no Istio objects in boutique"


def test_chaos_experiments() -> None:
    exp = {
        "metadata": {"name": "f03-redis-cart-pod-kill"},
        "spec": {"action": "pod-kill" + "x" * 1000},
        "status": {
            "experiment": {"desiredPhase": "Run"},
            "conditions": [{"type": "AllInjected", "status": "True"}],
        },
    }
    k, _ = reads(
        {
            "/podchaos": ok({"items": [exp]}),
            "/networkchaos": text("nf", 404),
            "/stresschaos": ok({"items": []}),
        }
    )
    out = k.chaos_experiments("boutique")
    assert "podchaos/f03-redis-cart-pod-kill" in out and "phase=Run AllInjected=True" in out
    assert "...(cut)" in out
    assert "networkchaos: Chaos Mesh not installed" in out
    broken, _ = reads({"/podchaos": text("denied", 403)})
    with pytest.raises(HttpError):
        broken.chaos_experiments("boutique")
    empty, _ = reads({"/": ok({"items": []})})
    assert empty.chaos_experiments("boutique") == "no chaos experiments in boutique"
