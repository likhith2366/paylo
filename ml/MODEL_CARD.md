# Fraud Risk Model — Model Card

XGBoost classifier scoring card charges in the synchronous risk path (§14.3).

**Trained on IEEE-CIS — real transactions. Test PR-AUC 0.3508.**

## The number

Only one row here is the deployed model. The others are reported because the
gap between them is the point.

| Model | Features | Test PR-AUC | Deployable |
|---|---|---|---|
| Guessing the base rate | — | 0.0344 | — |
| Hand-picked | 18 | 0.1772 | yes |
| **Servable** | **58** | **0.3508** | **yes — this is the real number** |
| Wide | 110 | 0.4898 | no |
| Wide + Vesta `V1`-`V339` | 449 | 0.4974 | no |

**Why 0.4898 is not the number.** It leans on `C1`-`C14` and `id_01`-`id_38`,
which carry 55% of that model's importance and which PayFlow cannot reproduce.
Vesta masked them deliberately; the competition says only "C1-C14: counting,
such as how many addresses are found to be associated with the payment card.
Actual meaning is masked." We can build a counter of the same shape, but not
`C8` specifically, and the model learns a weight for `C8` specifically.

Reporting 0.4898 would repeat the exact mistake documented under Serving below:
a number measured on features the charge path never sends.

**Vesta's 339 proprietary `V*` features are worth +0.008** on top of the wide
model. Inspecting them, 263 of 339 are integer-valued and they cluster into 11
blocks by missing-rate — they are entity-relationship counts, the same kind of
thing as `C*`, not something exotic. There is no large ceiling being left on
the table, and so nothing for a bigger model or a neural network to reach for.
(Tabular data is boosted-tree territory regardless; every strong solution on
this dataset is GBT, not a net.)

### Operating points — deployed model (test set)

| Recall | Precision | Caught | Missed | False alarms per 10k good charges |
|---|---|---|---|---|
| 50% | 0.267 | 2,032 | 2,032 | 488 |
| 70% | 0.159 | 2,845 | 1,219 | 1,319 |
| 90% | 0.080 | 3,658 | 406 | 3,711 |

50% recall is the usable point. 90% is not — declining 1 in 3 legitimate
customers to catch the last 10% of fraud is the wrong trade for almost any
merchant.

### Closing the gap the right way

0.3508 → 0.4898 is what an entity graph recovers: how many cards share this
device, how many emails share this address, how many addresses this card has
touched. That is §14.4, it is what `C*` and `V*` are, and it needs transaction
volume before the counts mean anything.

Built from our own traffic those features are strictly better than Vesta's,
because we would know what each one means and be able to compute it at
inference time. Until then the rule engine carries it — the device-card set and
velocity counters already in Redis are the first two edges of that graph.

## Data

**IEEE-CIS** (`ieee-fraud-detection`) — 590,540 real e-commerce transactions
from Vesta Corporation, 20,663 fraudulent (3.50%).

| Split | Rows |
|---|---|
| Train | 425,188 |
| Validation (tail of train, early stopping) | 47,244 |
| Test (latest 20% by time) | 118,108 |

**Previously trained on Sparkov and should not be.** That dataset is
synthetic, and its generator draws fraud amounts from category-specific bounded
intervals — no legitimate `grocery_pos` charge in 1.85M rows exceeds $290.88
while fraudulent ones start at $237. The resulting 0.9477 measured how well the
model reverse-engineered a simulator. Retraining after stripping those
artifacts collapsed it to 0.0989. It was chosen for convenience, not judgement.

**Also not used:** ULB `creditcardfraud`. Real, but PCA-anonymized to `V1`-`V28`,
so nothing can be computed from a live charge.

## Features (110)

| Group | Source | Reproducible by PayFlow? |
|---|---|---|
| Amount, time, velocity (18) | engineered here | yes — Redis counters |
| `C1`-`C14` | Vesta counting features | yes — same shape as our velocity counters |
| `D1`-`D15` | timedeltas since first/last seen | yes |
| `M1`-`M9` | name/address match flags | yes — a gateway computes these directly |
| `id_01`-`id_38`, `DeviceType`, `DeviceInfo` | device/identity | yes — our device fingerprint occupies this role |
| `card1`-`card6`, `addr1`-`addr2`, `dist1`-`dist2`, `ProductCD`, email domains | transaction attributes | yes |
| **`V1`-`V339`** | **Vesta proprietary** | **no — excluded** |

The `C*` counting features dominate the importances, which is the encouraging
part: they are exactly the shape of the velocity counters PayFlow already keeps
in Redis.

## Method

**Chronological split**, not random — a random split lets the model see a
card's later transactions while scoring its earlier ones.

**Imbalance handled, not removed** — `scale_pos_weight` rather than resampling,
which would teach a base rate that does not exist and make the probabilities
useless for setting a threshold.

**PR-AUC, not accuracy or ROC-AUC** — at a 3.4% base rate, predicting "never
fraud" scores 96.6% accuracy.

**Early stopping on a validation slice, never on test.**

**No leakage.** Rolling windows use `closed="left"` and per-card baselines use
`shift(1)`, so a transaction never contributes to the statistics used to score
it. Row order is restored and asserted before returning — an earlier version
sorted by card and returned rows in that order while the caller paired them
with labels in the original order, silently attaching every label to a
different transaction's features and producing a model that scored at chance.

## Serving

**The deployed model must be the model that was measured.** It briefly was not:
`internal/risk` sent 6 of 18 features and the rest defaulted silently, which
took the Sparkov model from 0.9735 offline to 0.2867 in production, or 0.0488
with a UTC/local timestamp mismatch on top.

**The model is never a hard dependency for payments.** On timeout or outage the
charge path fails open to the rule engine (`internal/risk`) behind a circuit
breaker.

## Known limitations

- **The Go client does not yet send the full 58-feature vector.** It sends the
  narrow engineered set, so production currently behaves closer to the 0.1772
  model. Wiring the remaining columns (`D1`-`D15`, `card1`-`card6`, `addr`,
  `dist`, email domains, device) is the next fix. `M1`-`M9` additionally need
  billing name and address, which checkout does not collect today.
- No adversarial adaptation — a static 2017-2018 snapshot.
- `card1` is a proxy for card identity; there is no true card id column.
- `TransactionDT` is a seconds offset from an undisclosed reference (taken as
  2017-12-01). Only hour-of-day and day-of-week derive from it, both invariant
  to that being off by whole days.
- Retraining is manual. §14.3 calls for weekly retraining on chargeback
  outcomes with new versions shadowed against live traffic first.
- Behavioral signals (paste detection, keystroke rhythm) are **rules** in
  `internal/risk/behavior.go`, not model features — no dataset contains
  keystroke data, so there is no weight to learn.
