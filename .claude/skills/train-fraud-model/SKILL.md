---
name: train-fraud-model
description: Download fraud datasets from Kaggle, engineer features, train the XGBoost risk model, and evaluate it correctly for a severely imbalanced problem. Use when working on fraud detection, risk scoring, the ML service, or model evaluation.
---

# Training the fraud model

Implements §14.3: a gradient-boosted tree scoring charges in under 10ms,
serving behind a hard timeout with a rule-engine fallback.

## Data

```bash
python ml/download_data.py              # both datasets
python ml/download_data.py --only sparkov
```

Needs a Kaggle token at `~/.kaggle/access_token` (new format) or
`~/.kaggle/kaggle.json` (legacy).

**sparkov** (`kartik2112/fraud-detection`) — 1.85M transactions, ~0.5% fraud.
Named columns: card number, amount, merchant category, cardholder DOB, and both
cardholder and merchant lat/long. **This is the servable dataset** — every
feature can be computed from a live charge at inference time.

**ieee** (`ieee-fraud-detection`) — 590k transactions, ~3.5% fraud, 434
features including email domain and device info. Richer, and the standard
public benchmark. Requires accepting the competition rules on Kaggle first, or
downloads return 403.

**Deliberately not used:** the ULB `creditcardfraud` set and its "balanced"
2023 variant. Their features are anonymized PCA components V1–V28, which cannot
be reconstructed from an incoming charge. A model trained on them scores well
offline and can never be deployed. Don't add them because they're popular.

## On class imbalance

Fraud genuinely runs well under 1% of transactions. That imbalance is the
problem, not a defect in the data.

**Do not resample to 50/50.** Oversampling or SMOTE teaches the model a base
rate that doesn't exist in production, and the resulting probabilities are
meaningless for setting a decline threshold.

Handle it in training instead:

- `scale_pos_weight = n_negative / n_positive` in XGBoost
- Evaluate with **PR-AUC**, not accuracy or ROC-AUC

Accuracy is actively misleading here: at 0.4% fraud, a model that predicts
"never fraud" scores 99.6%. ROC-AUC also flatters imbalanced classifiers
because the true-negative count swamps the false-positive rate. Precision-recall
is the honest curve.

Report **precision at fixed recall** — "at 70% recall we decline 1 in 200
legitimate charges" is the number a business can actually act on, and it's what
sets the decline threshold.

## Features

`ml/features.py`. The binding constraint: every feature must be computable from
one incoming charge plus cheap Redis lookups, inside a sub-100ms budget. That
rules out anything needing a scan of transaction history.

Groups: amount (raw, log, per-card z-score), temporal (hour, night, weekend),
velocity (1h/24h/7d counts from Redis counters), geo (haversine distance
between cardholder and merchant), cardholder age, and encoded categoricals.

**Leakage is the failure mode to watch for.** Every rolling window uses
`closed="left"` and every per-card baseline uses `shift(1)`, so a transaction
never contributes to the statistics used to score it. If validation numbers
look implausibly good, suspect leakage before celebrating.

**Split chronologically, never randomly.** A random split lets the model learn
from future transactions on the same card — the reported score will be far
better than production.

## Training

```bash
python ml/training/train.py --dataset sparkov
```

Writes to `ml/artifacts/`: the model, the categorical encodings, and a metrics
report. The encodings must ship with the model — regenerating them from
different data assigns different integers to the same category and silently
corrupts every prediction.

## Serving

FastAPI + ONNX Runtime behind gRPC with a strict timeout (§14.3).

**The ML service must never be a hard dependency for payments.** If it times
out or is down, fail open to the rule engine — never fail closed and block all
payments, and never skip risk checking entirely. That tradeoff is deliberate
and defensible; silently doing neither is not.

## Evaluating a new model version

Never swap a model in on offline metrics alone. Log both the old and new scores
on live traffic, compare offline, then shift traffic gradually (§14.3).

Sanity checks before trusting any result:

- Chronological split, not random
- No feature computed with information from the transaction's own future
- PR-AUC reported, not accuracy
- Precision at the operating threshold stated in business terms
- Feature importances are plausible — if an ID column dominates, that's leakage
