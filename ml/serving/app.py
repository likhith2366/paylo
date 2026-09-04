#!/usr/bin/env python3
"""Fraud model inference service (§14.3).

A deliberately thin FastAPI wrapper around the trained booster. Everything
expensive happens at startup; per-request work is one feature vector and one
`predict_proba` call, because this sits inside the synchronous charge path with
a sub-100ms budget for the whole risk step.

The contract with the Go payments service, which matters more than the model:

    This service is NEVER a hard dependency for payments.

The caller applies a hard timeout and falls back to the rule engine if this is
slow or down. Failing open to rules is correct; failing closed — blocking every
payment because a scoring service is unhealthy — would turn a model outage into
a total outage. That is the tradeoff §14.3 requires, and it is the caller's
job to enforce it, not this service's.

Run:
    uvicorn ml.serving.app:app --port 8000
"""

from __future__ import annotations

import json
import logging
import math
import time
from datetime import datetime
from pathlib import Path

import numpy as np
import xgboost as xgb
from fastapi import FastAPI
from pydantic import BaseModel, Field

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("fraud-service")

ARTIFACT_DIR = Path(__file__).resolve().parent.parent / "artifacts"

# Must match ml/features.py exactly. A mismatch here does not raise — it
# silently feeds the model the wrong column in each position, which produces
# confident nonsense. Asserted against the metrics file at startup.
FEATURE_COLUMNS = [
    "amt", "amt_log", "amt_zscore_card",
    "hour", "day_of_week", "is_night", "is_weekend",
    "txn_count_1h", "txn_count_24h", "txn_count_7d", "amt_sum_24h",
    "seconds_since_last_txn",
    "category_encoded", "state_encoded",
]

# Thresholds from the test-set operating points in metrics.json. Chosen for
# ~90% recall, then backed off slightly to reduce false declines — the
# asymmetry argued in internal/risk: a declined good customer is usually
# costlier than a missed fraudulent charge.
HIGH_RISK_THRESHOLD = 0.95
MEDIUM_RISK_THRESHOLD = 0.50


class ScoreRequest(BaseModel):
    """One charge to score.

    Every field is optional with a neutral default: the risk engine must be
    able to score a sparse transaction rather than erroring, because a missing
    device fingerprint should not fail a payment.
    """

    amount_cents: int = Field(..., ge=0)
    currency: str = "USD"
    timestamp: str | None = None

    card_fingerprint: str | None = None
    category: str | None = None
    state: str | None = None


    # Velocity counters, read from Redis by the caller — never recomputed here.
    txn_count_1h: float | None = None
    txn_count_24h: float | None = None
    txn_count_7d: float | None = None
    amt_sum_24h: float | None = None
    seconds_since_last_txn: float | None = None
    card_avg_amount: float | None = None
    card_std_amount: float | None = None


class ScoreResponse(BaseModel):
    score: float
    risk_level: str
    model_version: str
    latency_ms: float
    # True when key inputs were missing, so the caller can weigh the score
    # accordingly rather than treating a guess as a measurement.
    degraded: bool = False


class ModelBundle:
    """Model, encodings, and metadata, loaded once at startup."""

    def __init__(self, artifact_dir: Path):
        model_path = artifact_dir / "fraud_model.json"
        if not model_path.exists():
            raise FileNotFoundError(
                f"{model_path} not found. Train first: python ml/training/train.py"
            )

        self.booster = xgb.XGBClassifier()
        self.booster.load_model(model_path)

        encodings = json.loads((artifact_dir / "encodings.json").read_text())
        self.category_map: dict[str, int] = encodings["category_map"]
        self.state_map: dict[str, int] = encodings["state_map"]

        metrics_path = artifact_dir / "metrics.json"
        self.metrics = json.loads(metrics_path.read_text()) if metrics_path.exists() else {}
        self.version = self.metrics.get("trained_at", "unknown")

        # Serving and training must agree on column order. Catching this at
        # startup turns a silent scoring bug into a refusal to boot.
        trained_features = self.metrics.get("features")
        if trained_features and trained_features != FEATURE_COLUMNS:
            raise RuntimeError(
                "feature mismatch between training and serving:\n"
                f"  trained: {trained_features}\n"
                f"  serving: {FEATURE_COLUMNS}"
            )

        log.info(
            "loaded model version=%s features=%d test_pr_auc=%s",
            self.version, len(FEATURE_COLUMNS),
            self.metrics.get("test", {}).get("pr_auc", "?"),
        )


def build_vector(req: ScoreRequest, bundle: ModelBundle) -> tuple[np.ndarray, bool]:
    """Turn a request into the model's feature vector.

    Missing inputs become NaN rather than 0. XGBoost handles NaN natively by
    learning a default branch direction per split, whereas a 0 is a *claim* —
    "this card has had zero transactions in the last hour" — which is a
    different and often wrong statement from "we don't know".
    """
    degraded = False

    # Naive LOCAL time, deliberately. Training used the cardholder's wall clock,
    # and hour-of-day only means anything relative to their day — 3am matters
    # because they are asleep. An earlier version of the caller sent UTC, which
    # shifted hour by 4-8 hours and cost 0.24 PR-AUC on its own.
    ts = datetime.now()
    if req.timestamp:
        try:
            ts = datetime.fromisoformat(req.timestamp.replace("Z", "")).replace(tzinfo=None)
        except ValueError:
            degraded = True

    amt = req.amount_cents / 100.0

    # How unusual is this amount for this card, given its history.
    if req.card_avg_amount is not None and req.card_std_amount:
        amt_z = (amt - req.card_avg_amount) / req.card_std_amount
    else:
        amt_z = np.nan

    # Unseen categories map to -1, matching training. A new merchant category
    # must not take down the risk engine.
    category = bundle.category_map.get((req.category or "").lower(), -1)
    state = bundle.state_map.get((req.state or "").upper(), -1)

    if req.txn_count_1h is None:
        degraded = True

    # Order MUST match FEATURE_COLUMNS exactly. A mismatch does not raise — it
    # feeds the model the wrong column in each position and produces confident
    # nonsense. The startup check against metrics.json is what catches it.
    vector = [
        amt,
        math.log1p(amt),
        amt_z,
        ts.hour,
        ts.weekday(),
        1 if (ts.hour >= 22 or ts.hour <= 5) else 0,
        1 if ts.weekday() >= 5 else 0,
        _or_nan(req.txn_count_1h),
        _or_nan(req.txn_count_24h),
        _or_nan(req.txn_count_7d),
        _or_nan(req.amt_sum_24h),
        _or_nan(req.seconds_since_last_txn),
        category,
        state,
    ]
    return np.array([vector], dtype=np.float32), degraded


def _or_nan(value):
    return np.nan if value is None else value


app = FastAPI(title="PayFlow Fraud Scoring", version="1.0.0")
bundle: ModelBundle | None = None


@app.on_event("startup")
def load_model() -> None:
    global bundle
    bundle = ModelBundle(ARTIFACT_DIR)


@app.get("/healthz")
def healthz():
    return {"status": "ok", "model_loaded": bundle is not None}


@app.get("/readyz")
def readyz():
    if bundle is None:
        return {"status": "not_ready"}
    return {"status": "ready", "model_version": bundle.version}


@app.get("/model")
def model_info():
    """Metadata and honest performance figures.

    Serves the model card's caveat alongside the number, so nobody reads
    PR-AUC 0.97 off a dashboard and mistakes it for real-world accuracy.
    """
    if bundle is None:
        return {"error": "model not loaded"}
    test = bundle.metrics.get("test", {})
    return {
        "version": bundle.version,
        "features": FEATURE_COLUMNS,
        "test_pr_auc": test.get("pr_auc"),
        "test_roc_auc": test.get("roc_auc"),
        "baseline_pr_auc": test.get("baseline_pr_auc"),
        "caveat": (
            "Trained on synthetic data (Sparkov) whose generator concentrates "
            "84% of fraud into 22:00-04:00 and uses distinctly higher amounts. "
            "The high PR-AUC reflects reverse-engineering that generator, not "
            "real-world fraud detection skill. See ml/MODEL_CARD.md."
        ),
    }


@app.post("/score", response_model=ScoreResponse)
def score(req: ScoreRequest) -> ScoreResponse:
    started = time.perf_counter()

    if bundle is None:
        # Should be unreachable — readiness gates traffic. Returning a neutral
        # score rather than raising keeps a caller without a circuit breaker
        # from seeing a 500 on the charge path.
        return ScoreResponse(score=0.0, risk_level="low", model_version="unloaded",
                             latency_ms=0.0, degraded=True)

    vector, degraded = build_vector(req, bundle)
    probability = float(bundle.booster.predict_proba(vector)[0, 1])

    if probability >= HIGH_RISK_THRESHOLD:
        level = "high"
    elif probability >= MEDIUM_RISK_THRESHOLD:
        level = "medium"
    else:
        level = "low"

    return ScoreResponse(
        score=round(probability, 6),
        risk_level=level,
        model_version=bundle.version,
        latency_ms=round((time.perf_counter() - started) * 1000, 3),
        degraded=degraded,
    )
