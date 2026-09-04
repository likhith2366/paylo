# Fraud Risk Model — Model Card

XGBoost classifier scoring card charges in the synchronous risk path (§14.3).

**Current model: 14 features, test PR-AUC 0.9477.**

Every number below is labelled with the split it comes from. An earlier version
of this card contained five statistics that did not reproduce from any split;
they have been regenerated.

---

## Read this before quoting the headline

PR-AUC 0.9477 is not evidence of a good fraud model. It measures how completely
the model reverse-engineered a simulator.

The training data (`kartik2112/fraud-detection`, "Sparkov") is **synthetic**,
and the generator draws fraud amounts from **separate, category-specific,
bounded intervals**:

| Category | Fraud amount range | Legit amount range | Overlap |
|---|---|---|---|
| `grocery_pos` | $237.04 – $397.97 | $10.74 – **$290.88** | **no legit row above $290.88, ever** |
| `shopping_net` | $711.20 – $1312.98 | median $8.30 | narrow high band |
| `misc_net` | $576.10 – $1112.91 | median $9.73 | narrow high band |
| `gas_transport` | **$5.53 – $24.84** | median $62.94 | fraud is *lower* — a pure tell |

Two `(category, amount-band)` cells are **100% fraud on the test set** (250/250
rows, zero false positives) and cover 18.5% of all test fraud. `grocery_pos ≥
$300` is 1,458 fraud and **0** legit across all 1.85M rows. That single fact
produces the "precision 0.997, 2 false positives" operating point below.

**Fraud also arrives in runs.** P(previous transaction on this card was fraud |
this one is fraud) = **0.907**, versus 0.0005 for legitimate charges. Runs
average 10.9 consecutive authorizations. Real fraud interleaves with the
cardholder's own traffic; this does not. Decomposing the 18-feature model on
test: frauds *first in their run* score PR-AUC **0.806**, frauds *later in a
run* score **0.978**. The headline is dominated by the easy 90%.

**The population is not a population.** 999 distinct cards, 693 merchants, and
**97.7% of cards get frauded** at some point.

### What removing the artifacts does — measured, full retrains

On the 18-feature model, same split, same hyperparameters:

| Data | Test PR-AUC | Test ROC-AUC |
|---|---|---|
| Unmodified (control) | 0.9735 | 0.9995 |
| Hour-of-day randomised (night rule destroyed) | 0.9017 | 0.9978 |
| Fraud amounts resampled from same-category legit | **0.4744** | 0.9464 |
| Both removed | **0.0989** | 0.8041 |

The amount artifact is worth ~0.50 PR-AUC. The night artifact ~0.07.

**A realistic landing zone for this pipeline on non-synthetic data is 0.10–0.47.**
For comparison, the standard public benchmark (ULB/Worldline, Dal Pozzolo et
al.) reports ≈0.74–0.81, and proprietary production streams score lower.

### A correction worth recording

An earlier version of this card blamed the night concentration as the primary
cause, citing XGBoost's **gain** importance which ranked `is_night` second at
0.192. That was wrong. Test-time **permutation** importance — the drop in
PR-AUC when a column is shuffled — tells a different story:

| Feature | Gain importance (misleading) | Permutation importance (real) |
|---|---|---|
| `amt` | 0.342 | **0.817** |
| `category_encoded` | 0.062 | **0.463** |
| `amt_sum_24h` | 0.149 | 0.261 |
| `hour` | 0.020 | 0.093 |
| `is_night` | **0.192** | **0.022** |
| `distance_km` | 0.014 | **0.0005** |

Gain importance counts how often a feature is split on. Permutation importance
measures whether the prediction actually depends on it. Quote the second.

---

## Serving reality

**The deployed model must be the model that was measured.** It briefly was not,
and the gap was severe.

The Go client originally sent 6 of the 18 features. The rest defaulted to `-1`
or `NaN` inside the scoring service, silently — including `category_encoded`,
the second most important feature.

| Feature vector | Test PR-AUC | Recall @0.95 | Precision @0.95 |
|---|---|---|---|
| What the card reported (18 features) | 0.9735 | 0.900 | 0.967 |
| **What production actually sent (6)** | **0.2867** | **0.391** | **0.319** |
| …plus a UTC/local timestamp mismatch | **0.0488** | — | — |

At the deployed threshold, production caught 39% of fraud and produced 1,127
false positives instead of 41.

**Resolution.** The feature set was cut to the 14 the charge path can genuinely
supply, and `internal/risk` now sends all 14. The dropped four:

| Feature | Why it is gone |
|---|---|
| `age_years` | Needs cardholder date of birth. No checkout collects it. |
| `city_pop_log` | Needs the population of their home city. Unknown to a gateway. |
| `gender_encoded` | Protected attribute. Serving also hardcoded it to `0`, asserting one value for every cardholder. |
| `distance_km` | Unlearnable here — see below. |

Cost of dropping them: 0.9735 → **0.9477**. Cheap, and now honest.

The timestamp is sent as naive **local** time, not UTC. Hour-of-day only means
anything relative to the cardholder's day; 3am matters because they are asleep,
not because of an offset from Greenwich.

### The `distance_km` claim was false

`features.py` previously asserted that distance from home was "the single
strongest simple signal in this dataset." It is the weakest feature in the
model. Sparkov places every merchant a small random offset from the cardholder:

| | Fraud | Legit |
|---|---|---|
| Median distance | 78.1 km | 78.2 km |
| Max distance | 144.5 km | 152.1 km |
| Rows over 1000 km | **0** | **0** |

Single-feature AUC 0.5044 — a coin flip. In the real world geo distance
matters; it cannot be learned from this data.

---

## What the model is actually worth over simpler approaches

An earlier version of this card said "a single threshold `amount > $200`
catches 75% of fraud at a 4.3% false-positive rate, with no model at all,"
implying the ML adds little. The fact is true; the implication is wrong. That
rule's **precision is 8.4%** and its PR-AUC is **0.1014**.

Test PR-AUC on the same chronological split (base rate 0.0036):

| Model | Test PR-AUC |
|---|---|
| Guess the base rate | 0.0036 |
| Best single `amt` threshold (`amt ≥ $543`) | 0.1014 |
| 2 rules: depth-2 tree on (`amt`, `is_night`) | 0.1605 |
| Logistic regression, all features | 0.3107 |
| Depth-6 decision tree, all features | 0.4406 |
| Best hand-built lookup table (designed *after* seeing the generator) | 0.6847 |
| XGBoost, 3 features (`amt`, `is_night`, `category`) | 0.7297 |
| **XGBoost, 14 features (current)** | **0.9477** |
| XGBoost, 18 features (previous, unservable) | 0.9735 |

The honest framing: **the model does a much better job — at reverse-engineering
a simulator.**

---

## Data

`kartik2112/fraud-detection` — 1,852,394 transactions, 9,651 fraudulent
(0.521%).

| Split | Rows | Fraud rate |
|---|---|---|
| Train | 1,333,723 | 0.575% |
| Validation (tail of train, for early stopping) | 148,192 | — |
| Test (latest 20% by time) | 370,479 | 0.364% |

Split boundary: 2020-08-25 01:31:59.

**Deliberately not used:** the ULB `creditcardfraud` dataset and its "balanced"
2023 variant. Their features are anonymized PCA components `V1`–`V28`, which
cannot be reconstructed from an incoming charge — a model trained on them
cannot be served, however well it scores offline.

---

## Method

**Chronological split, not random.** A random split lets the model see a card's
later transactions while scoring its earlier ones.

**Imbalance handled, not removed.** `scale_pos_weight = 173.0` rather than
resampling to 50/50, which would teach a base rate that does not exist and make
the output probabilities useless for setting a threshold.

**PR-AUC, not accuracy or ROC-AUC.** At a 0.5% base rate, predicting "never
fraud" scores 99.5% accuracy. ROC-AUC flatters imbalanced classifiers because
the large true-negative count suppresses the false-positive rate.

**Early stopping evaluates on a validation slice, never on test.** It
previously used the test set, which leaks model selection into the reported
number. It happened to cost nothing — `best_iteration` was 399 of 400, so it
never fired, and a fixed-400 run scored identically — but it was a latent bug
that would have silently corrupted the headline the moment the model did stop
early. Now fixed; the current model stopped at round 237.

### No leakage — verified, not asserted

Four independent checks:

1. **Row alignment.** `build_features` given a slice with a non-zero-based
   index and a shuffled index returns rows in the caller's order both times.
   Asserted in code before returning.
2. **Prefix invariance — the decisive test.** For a card with 4,392
   transactions, features built on the full history versus on only the first
   *k* rows (k = 10/100/1000/3000) differ by **0.0 across every numeric feature
   at every k**. If any feature for row N used row N or later, these would
   diverge. Repeated on a 40,000-row multi-card slice: all zero.
3. **`closed="left"` verified directly** — a transaction never enters its own
   window, and even a duplicate-timestamp prior transaction is excluded.
4. **Sanity.** 13.4% of test rows have `amt_sum_24h < amt`, impossible if the
   current amount were included. `corr(amt, amt_sum_24h) = 0.057`.

### The bug that produced a coin-flip model

The first full training run scored **PR-AUC 0.0039, ROC-AUC 0.514**, and
XGBoost's early stopping halted at round 1.

`build_features` sorted rows by `cc_num` for the rolling velocity windows and
returned them in that order, while the caller paired the output with a `y`
taken in the original order. Every label was attached to a different
transaction's features.

Measured on a 20,000-row slice:

| Mean amount | Correct pairing | As actually trained |
|---|---|---|
| Fraud | $554.42 | $63.79 |
| Legitimate | $67.26 | $68.93 |

An 8× separation became noise. Nothing errored; the run completed and wrote
artifacts.

**The general lesson:** a model that trains cleanly and scores near the base
rate almost always has a data-plumbing bug, not a hyperparameter problem. Check
alignment before touching anything else.

---

## Features (14)

All computable at inference time from one charge plus Redis counters, within
the sub-100ms risk budget. That constraint is the point — see "Serving reality"
for what happens when it is violated.

| Group | Features |
|---|---|
| Amount | `amt`, `amt_log`, `amt_zscore_card` |
| Temporal | `hour`, `day_of_week`, `is_night`, `is_weekend` |
| Velocity | `txn_count_1h/24h/7d`, `amt_sum_24h`, `seconds_since_last_txn` |
| Categorical | `category_encoded`, `state_encoded` |

A count of zero is stored as `0`, not `NaN`. "No charges in the last hour" is a
fact, not missing data, and Redis returns `0` for an absent key — encoding it
as `NaN` in training created a train/serve mismatch affecting 83% of rows.

---

## Operating points (test set, current 14-feature model)

| Recall | Threshold | Precision | Caught | Missed | False alarms per 10k good charges |
|---|---|---|---|---|---|
| 50% | 0.9996 | 0.997 | 675 | 674 | 0.1 |
| 70% | 0.9982 | 0.991 | 944 | 405 | 0.2 |
| 90% | 0.9340 | 0.834 | 1,214 | 135 | 6.5 |

These describe the deployed system, now that the feature vectors match. **On
real data expect them to be dramatically worse** — see the de-artifacted
retrains above.

---

## Serving

FastAPI, loaded from `ml/artifacts/fraud_model.json` plus `encodings.json`. The
encodings must ship with the model: regenerating them from different data
assigns different integers to the same category and silently corrupts every
prediction. The service refuses to start if its `FEATURE_COLUMNS` disagree with
the trained model's.

**The model is never a hard dependency for payments.** On timeout or outage the
charge path fails open to the rule engine (`internal/risk`) behind a circuit
breaker — never fails closed and blocks all payments, and never skips risk
checking entirely.

---

## Known limitations

- **Synthetic data.** The headline does not transfer. This is the first thing
  to say about this model, not the last.
- **No adversarial adaptation.** Real fraud shifts in response to detection;
  this is a static 2019–2020 snapshot.
- **999 cards, 693 merchants.** Not a population.
- US-only data and geography.
- No merchant-level risk history, which needs production traffic to build.
- Retraining is manual. §14.3 calls for weekly retraining on chargeback
  outcomes, with new versions shadowed against live traffic before rollout.
- Behavioral signals (paste detection, keystroke rhythm) are implemented as
  **rules** in `internal/risk/behavior.go`, not model features — this dataset
  contains no keystroke data, so there is no weight to learn.
