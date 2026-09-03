# Fraud Risk Model — Model Card

XGBoost classifier scoring card charges in the synchronous risk path (§14.3).

## Headline number, and why you should discount it

**Test PR-AUC 0.9735** on a chronological hold-out.

That number is not evidence of a good fraud model, and it should not be quoted
without this section attached.

The training data (`kartik2112/fraud-detection`, "Sparkov") is **synthetically
generated**, and the generator's rules are visible in the data:

| Signal | Fraud | Legitimate |
|---|---|---|
| Median amount | $371.94 | $47.15 |
| Share falling 22:00–03:59 | **84.2%** | 23.2% |

Real card fraud does not concentrate 84% of its volume into six hours. A single
hand-written threshold — `amount > $200` — catches 75% of the fraud in this
dataset at a 4.3% false-positive rate, with no model at all.

The model's learned feature importances are the generator's rules read back:

```
amt              0.342
is_night         0.192
amt_sum_24h      0.149
amt_log          0.116
category         0.062
```

So PR-AUC 0.97 measures how completely the model reverse-engineered a
simulator. A production model on real traffic would land far lower — published
work on real card fraud typically reports PR-AUC in the 0.3–0.7 range, and
that is the range to expect here on real data.

**What the number does legitimately demonstrate:** the pipeline is correct
end to end — features are computed without leakage, the split is chronological,
imbalance is handled without resampling, and the model learns the signal that
is genuinely present. That is what a portfolio model can honestly claim.

## The bug that produced the first result

The first full training run scored **PR-AUC 0.0039, ROC-AUC 0.514** — a coin
flip — and XGBoost's early stopping halted at round 1.

`build_features` sorted rows by `cc_num` for the rolling velocity windows and
returned them in that order, while the caller paired the output with a `y`
taken in the original order. Every label was attached to a different
transaction's features.

The damage, measured:

| Mean amount | Correct pairing | As actually trained |
|---|---|---|
| Fraud | $554.42 | $63.79 |
| Legitimate | $67.26 | $68.93 |

An 8× separation became noise. Nothing errored; the run completed and wrote
artifacts. `build_features` now restores the caller's row order and asserts it
before returning.

The general lesson, worth keeping: a model that trains cleanly and scores near
the base rate is far more likely to have a data-plumbing bug than to need
tuning. Check alignment before touching hyperparameters.

## Data

`kartik2112/fraud-detection` — 1,852,394 transactions, 9,651 fraudulent
(0.521%).

- Train: 1,481,915 rows (earliest 80% by time)
- Test: 370,479 rows (latest 20%)
- Split boundary: 2020-08-25

**Deliberately not used:** the ULB `creditcardfraud` dataset and its "balanced"
2023 variant. Their features are anonymized PCA components `V1`–`V28`, which
cannot be reconstructed from an incoming charge — a model trained on them
cannot be served, however well it scores offline.

## Method

**Chronological split, not random.** A random split lets the model see a card's
later transactions while scoring its earlier ones. The reported score is then
better than production, and the gap only surfaces after deployment.

**Imbalance handled, not removed.** `scale_pos_weight = 177.5` rather than
resampling to 50/50. Resampling teaches a base rate that does not exist,
making the output probabilities useless for setting a decline threshold.

**PR-AUC, not accuracy or ROC-AUC.** At a 0.5% base rate, predicting "never
fraud" scores 99.5% accuracy. ROC-AUC also flatters imbalanced classifiers
because the large true-negative count suppresses the false-positive rate.

**No leakage in the features.** Every rolling window uses `closed="left"` and
every per-card baseline uses `shift(1)`, so a transaction never contributes to
the statistics used to score it.

## Features (18)

All computable at inference time from one charge plus Redis counters, within
the sub-100ms risk budget.

| Group | Features |
|---|---|
| Amount | `amt`, `amt_log`, `amt_zscore_card` |
| Temporal | `hour`, `day_of_week`, `is_night`, `is_weekend` |
| Velocity | `txn_count_1h/24h/7d`, `amt_sum_24h`, `seconds_since_last_txn` |
| Geo | `distance_km` (haversine, cardholder to merchant), `city_pop_log` |
| Cardholder | `age_years` |
| Categorical | `category_encoded`, `state_encoded`, `gender_encoded` |

`gender_encoded` is present because it is in the source data and the generator
uses it. **It should be removed before any real deployment** — scoring
creditworthiness or fraud risk on a protected attribute invites both a
discrimination claim and a regulator's attention, regardless of predictive
value. It is retained here only to keep the offline benchmark comparable.

## Operating points (test set)

| Recall | Threshold | Precision | Caught | Missed | False alarms per 10k good charges |
|---|---|---|---|---|---|
| 50% | 0.9998 | 1.000 | 675 | 674 | 0.0 |
| 70% | 0.9990 | 0.999 | 944 | 405 | 0.0 |
| 90% | 0.9504 | 0.967 | 1,214 | 135 | 1.1 |

These are the numbers a business acts on — "we catch 90% of fraud and
inconvenience 1 legitimate customer in 9,000" is actionable in a way an AUC
is not. On real data expect these to be dramatically worse.

## Serving

FastAPI, loaded from `ml/artifacts/fraud_model.json` plus `encodings.json`.
The encodings must ship with the model: regenerating them from different data
assigns different integers to the same category and silently corrupts every
prediction.

**The model is never a hard dependency for payments.** On timeout or outage the
charge path fails open to the rule engine (`internal/risk`) — never fails
closed and blocks all payments, and never skips risk checking entirely. That
tradeoff is deliberate and defensible; silently doing neither is not.

## Known limitations

- Synthetic data, as above. The headline metric does not transfer.
- No adversarial adaptation. Real fraud shifts in response to detection; this
  is a static snapshot from 2019–2020.
- `gender_encoded` must be dropped before real use.
- Trained on US-only data with US geography.
- No merchant-level features. A real model would use merchant category risk
  and per-merchant fraud history, which need production traffic to build.
- Retraining is manual. §14.3 calls for weekly retraining on chargeback
  outcomes, with new versions shadowed against live traffic before rollout.
