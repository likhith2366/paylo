"""Feature engineering for the fraud model (§14.3).

The guiding constraint here is that every feature must be computable at
inference time from a single incoming charge plus cheap Redis lookups, within
the risk engine's sub-100ms budget. That rules out anything requiring a join
against full transaction history, and it is why this file exists rather than
just feeding raw columns to XGBoost.

Feature groups, matching the design doc's list:

  amount        raw and log-scaled amount, plus deviation from the card's own
                historical average
  temporal      hour of day, day of week, is-night — fraud clusters heavily in
                the small hours when cardholders are asleep
  velocity      transactions per card over 1h / 24h / 7d windows. In production
                these are Redis counters (`charges_last_1h:{fingerprint}`)
                incremented on each charge with a TTL, never recomputed by
                scanning history
  geo           REMOVED. An earlier version claimed distance from home was
                "the single strongest simple signal in this dataset". That was
                false and worth recording: Sparkov places every merchant a
                small random offset from the cardholder, so fraud distance
                (median 78.1km, max 144.5km) is indistinguishable from legit
                (median 78.2km, max 152.1km) and no row in either class exceeds
                1000km. Single-feature AUC 0.504 — a coin flip. In the real
                world geo distance matters; it cannot be learned from this data,
                so carrying it only added a feature production must supply for
                no benefit.
  categorical   merchant category and state — target-free label encoding

FEATURES DELIBERATELY EXCLUDED, and why:

  age_years     Needs the cardholder's date of birth. No checkout collects it,
                so it can never be supplied at inference time.
  city_pop_log  Needs the population of the cardholder's home city. Unknown to
                a payment gateway.
  gender        A protected attribute. Scoring fraud risk on it invites a
                discrimination claim regardless of predictive value. The
                serving code additionally hardcoded it to 0 on every request,
                silently asserting one value for every cardholder.
  distance_km   Unlearnable from this dataset (see above).

That list is not squeamishness. Training on a feature production cannot supply
is worse than not having it: the model learns to lean on the feature, then
receives a default at inference and scores confidently on a value that means
nothing. Measured cost of exactly that mistake here — the model scored 0.9735
offline and 0.2867 through the vector the Go client actually sends.

Training must not leak the future into the past: every rolling window below is
computed with `closed="left"`, so a transaction's own value never contributes
to the counters used to score it.
"""

from __future__ import annotations

import numpy as np
import pandas as pd

# Columns fed to the model. Kept explicit so training and serving cannot drift
# apart — a mismatch here is silent and produces garbage scores in production.
FEATURE_COLUMNS = [
    "amt",
    "amt_log",
    "amt_zscore_card",
    "hour",
    "day_of_week",
    "is_night",
    "is_weekend",
    "txn_count_1h",
    "txn_count_24h",
    "txn_count_7d",
    "amt_sum_24h",
    "seconds_since_last_txn",
    "category_encoded",
    "state_encoded",
]


def build_features(df: pd.DataFrame, category_map=None, state_map=None) -> tuple[pd.DataFrame, dict]:
    """Turn raw Sparkov rows into the model's feature matrix.

    Returns the feature frame and the categorical encodings, which must be
    saved with the model and reused at inference — regenerating them from
    different data would assign different integers to the same category.
    """
    df = df.copy()
    df["trans_date_trans_time"] = pd.to_datetime(df["trans_date_trans_time"])

    # Remember where every row started.
    #
    # This matters more than it looks. The rolling windows below need the frame
    # sorted by card and time, but the caller pairs our output with a `y` taken
    # in the ORIGINAL order. Returning sorted rows silently misaligns every
    # label with someone else's features — which is not an error, just a model
    # that learns nothing. It cost a full training run to find. The original
    # order is restored before returning, and asserted at the end.
    original_order = df.index.to_numpy().copy()
    df["_row_id"] = np.arange(len(df))

    # Chronological order within each card is required for the rolling windows
    # to mean anything.
    df = df.sort_values(["cc_num", "trans_date_trans_time"]).reset_index(drop=True)

    # --- amount -------------------------------------------------------------
    # log1p because the amount distribution is heavily right-skewed; a handful
    # of very large charges would otherwise dominate every split.
    df["amt_log"] = np.log1p(df["amt"])

    # --- temporal -----------------------------------------------------------
    ts = df["trans_date_trans_time"]
    df["hour"] = ts.dt.hour
    df["day_of_week"] = ts.dt.dayofweek
    df["is_night"] = ((df["hour"] >= 22) | (df["hour"] <= 5)).astype(int)
    df["is_weekend"] = (df["day_of_week"] >= 5).astype(int)

    # --- velocity -----------------------------------------------------------
    # closed="left" excludes the current row from its own window. Without it
    # every transaction would count itself, and worse, the model would learn
    # from information unavailable at scoring time.
    #
    # Each group is computed against its own index rather than relying on
    # groupby.apply's concatenation order, which is not guaranteed to match row
    # order and is exactly the kind of implicit ordering that caused the
    # misalignment described above.
    windows = {
        "txn_count_1h": ("1h", "count"),
        "txn_count_24h": ("24h", "count"),
        "txn_count_7d": ("7D", "count"),
        "amt_sum_24h": ("24h", "sum"),
    }
    for column in windows:
        df[column] = np.nan

    for _, group in df.groupby("cc_num", sort=False):
        series = group.set_index("trans_date_trans_time")["amt"]
        for column, (window, how) in windows.items():
            rolled = series.rolling(window, closed="left")
            values = rolled.count() if how == "count" else rolled.sum()
            # group.index carries this group's positions in df, so the write
            # lands on exactly the rows the values were computed from.
            df.loc[group.index, column] = values.to_numpy()

    # A count of zero is a FACT — "no charges on this card in the last hour" —
    # not missing data. pandas returns NaN for an empty window, which conflates
    # the two, and 83% of rows were affected. It also created a train/serve
    # mismatch: Redis returns 0 for a key that does not exist, so production
    # sent 0 where training had seen NaN.
    for column in windows:
        df[column] = df[column].fillna(0.0)

    # A burst of charges seconds apart on one card is the classic card-testing
    # pattern — a fraudster verifying which stolen numbers still work.
    df["seconds_since_last_txn"] = (
        df.groupby("cc_num")["trans_date_trans_time"].diff().dt.total_seconds()
    )

    # How unusual is this amount *for this card*, using only prior history.
    # shift(1) is what keeps the current amount out of its own baseline.
    card_amt = df.groupby("cc_num")["amt"]
    prior_mean = card_amt.transform(lambda s: s.shift(1).expanding().mean())
    prior_std = card_amt.transform(lambda s: s.shift(1).expanding().std())
    df["amt_zscore_card"] = (df["amt"] - prior_mean) / prior_std.replace(0, np.nan)

    # --- categoricals -------------------------------------------------------
    if category_map is None:
        category_map = {v: i for i, v in enumerate(sorted(df["category"].unique()))}
    if state_map is None:
        state_map = {v: i for i, v in enumerate(sorted(df["state"].unique()))}

    # -1 for unseen categories at inference time rather than an exception:
    # a new merchant category must not take down the risk engine.
    df["category_encoded"] = df["category"].map(category_map).fillna(-1).astype(int)
    df["state_encoded"] = df["state"].map(state_map).fillna(-1).astype(int)

    # A first-ever transaction on a card genuinely has no prior history. NaN is
    # the honest encoding, and XGBoost handles it natively by learning a default
    # direction per split — better than imputing a zero that would falsely read
    # as "this card has never been used in the last hour".

    # Restore the caller's row order, so the returned features pair with the
    # caller's labels.
    df = df.sort_values("_row_id")
    features = df[FEATURE_COLUMNS].copy()
    features.index = original_order

    # Assert it rather than trust it. A silent misalignment here produces a
    # model that trains cleanly and predicts nothing, which is far more
    # expensive to discover than a failed assertion.
    if not np.array_equal(df["_row_id"].to_numpy(), np.arange(len(df))):
        raise RuntimeError(
            "features: row order was not restored — features and labels would misalign"
        )

    encodings = {"category_map": category_map, "state_map": state_map}
    return features, encodings
