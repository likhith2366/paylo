"""Feature engineering for IEEE-CIS (real Vesta e-commerce transactions).

Real data, unlike Sparkov. 590,540 transactions, 20,663 fraudulent (3.50%) —
fewer rows than the synthetic set but more than twice the fraud examples, which
is the scarce resource when training this.

Same constraint as always: every feature must be computable at inference time
from one charge plus Redis counters. That rules out most of what makes this
dataset famous.

WHAT IS DELIBERATELY LEFT ON THE TABLE

  V1-V339   Vesta's own engineered features, and by far the strongest signal in
            the dataset. They are opaque — nobody outside Vesta knows what they
            compute — so they cannot be reproduced from an incoming charge.
            Kaggle solutions lean on them heavily, which is exactly why Kaggle
            scores on this dataset do not transfer to a deployable system.
  C1-C14    Vesta's counting features. Similar shape to our Redis velocity
            counters but with undocumented definitions, so we compute our own
            instead of inheriting numbers we cannot reproduce.
  D1-D15    Timedelta features, same reasoning. D1 (days since previous
            transaction on this card) overlaps our seconds_since_last_txn,
            which we compute ourselves.

Using them would raise the score and produce a model PayFlow cannot serve —
the exact mistake that made the Sparkov model read 0.97 offline and 0.29 in
production.

CARD IDENTITY. There is no card id column. `card1` is the standard proxy: a
hashed card attribute stable enough to group a card's history, which is what
the velocity windows need.

TIMESTAMPS. `TransactionDT` is a seconds offset from an undisclosed reference.
The competition-established reference is 2017-12-01; only hour-of-day and
day-of-week are derived from it, and both are invariant to the reference being
off by whole days.
"""

from __future__ import annotations

import numpy as np
import pandas as pd

REFERENCE_DATE = pd.Timestamp("2017-12-01")

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
    "card_brand_encoded",
    "card_type_encoded",
    "email_domain_encoded",
    "product_encoded",
    "addr_encoded",
    "device_type_encoded",
]

# Every one of these maps to something PayFlow actually holds at charge time.
_SOURCE = {
    "card_brand_encoded": "card4",         # visa / mastercard / amex / discover
    "card_type_encoded": "card6",          # debit / credit — derivable from BIN
    "email_domain_encoded": "P_emaildomain",
    "product_encoded": "ProductCD",        # merchant category analogue
    "addr_encoded": "addr1",               # billing region
    "device_type_encoded": "DeviceType",
}


def build_features(df: pd.DataFrame, maps: dict | None = None) -> tuple[pd.DataFrame, dict]:
    """Build the feature matrix. Returns features in the caller's row order.

    Row order is restored before returning and asserted — the velocity windows
    need the frame sorted by card and time, and returning sorted rows would
    silently pair every label with a different transaction's features.
    """
    df = df.copy()
    original_order = df.index.to_numpy().copy()
    df["_row_id"] = np.arange(len(df))

    df["_ts"] = REFERENCE_DATE + pd.to_timedelta(df["TransactionDT"], unit="s")
    df["_card"] = df["card1"].fillna(-1)

    df = df.sort_values(["_card", "_ts"]).reset_index(drop=True)

    df["amt"] = df["TransactionAmt"]
    df["amt_log"] = np.log1p(df["amt"])

    ts = df["_ts"]
    df["hour"] = ts.dt.hour
    df["day_of_week"] = ts.dt.dayofweek
    df["is_night"] = ((df["hour"] >= 22) | (df["hour"] <= 5)).astype(int)
    df["is_weekend"] = (df["day_of_week"] >= 5).astype(int)

    # Velocity, computed per card. closed="left" keeps a transaction out of its
    # own window — without it the model learns from information it will not
    # have at scoring time.
    windows = {
        "txn_count_1h": ("1h", "count"),
        "txn_count_24h": ("24h", "count"),
        "txn_count_7d": ("7D", "count"),
        "amt_sum_24h": ("24h", "sum"),
    }
    for column in windows:
        df[column] = np.nan

    for _, group in df.groupby("_card", sort=False):
        series = group.set_index("_ts")["amt"]
        for column, (window, how) in windows.items():
            rolled = series.rolling(window, closed="left")
            values = rolled.count() if how == "count" else rolled.sum()
            df.loc[group.index, column] = values.to_numpy()

    # An empty window means zero charges, which is a fact, not missing data.
    # Redis returns 0 for an absent key, so encoding it as NaN here would
    # recreate the train/serve mismatch.
    for column in windows:
        df[column] = df[column].fillna(0.0)

    df["seconds_since_last_txn"] = df.groupby("_card")["_ts"].diff().dt.total_seconds()

    # shift(1) keeps the current amount out of its own baseline.
    card_amt = df.groupby("_card")["amt"]
    prior_mean = card_amt.transform(lambda s: s.shift(1).expanding().mean())
    prior_std = card_amt.transform(lambda s: s.shift(1).expanding().std())
    df["amt_zscore_card"] = (df["amt"] - prior_mean) / prior_std.replace(0, np.nan)

    # Categoricals. Fitted on train only; -1 for anything unseen, so a new
    # email domain or device cannot take down the risk engine.
    if maps is None:
        maps = {
            name: {v: i for i, v in enumerate(sorted(df[source].dropna().astype(str).unique()))}
            for name, source in _SOURCE.items()
        }
    for name, source in _SOURCE.items():
        df[name] = df[source].astype(str).map(maps[name]).fillna(-1).astype(int)

    df = df.sort_values("_row_id")
    if not np.array_equal(df["_row_id"].to_numpy(), np.arange(len(df))):
        raise RuntimeError("features_ieee: row order not restored — labels would misalign")

    features = df[FEATURE_COLUMNS].copy()
    features.index = original_order
    return features, maps


# Columns PayFlow's Go client genuinely cannot supply.
#
# Not squeamishness — these are Vesta's own masked features. The competition
# says only "C1-C14: counting, actual meaning is masked" and "V1-V339: Vesta
# engineered rich features". We can build counters of the same SHAPE, but not
# C8 specifically, and the model learns a weight for C8 specifically. Sending
# our own numbers under those names is the same train/serve skew that took the
# previous model from 0.97 offline to 0.29 in production, just subtler.
#
# The replacement is our own entity graph (§14.4) once there is traffic to
# build it from — and ours will be better, because we will know what each
# number means and be able to compute it at inference time.
UNSERVABLE_PREFIXES = ("C", "V", "id_")


# Columns whose MEANING we know and whose value PayFlow can compute from a
# live charge. This is a stricter test than "not obviously proprietary".
#
# Excluded even though they survive the C/V/id filter:
#   card1,2,3,5  hashed Vesta card attributes. card1 works as a card-identity
#                proxy for grouping history, but we cannot produce its value
#                for a new charge — we do not know what it encodes.
#   dist1,2      masked distances, undocumented.
#   D1-D15       "timedelta", but which entity each measures is masked.
#   M1-M9        match flags. Conceptually reproducible — does the name on the
#                card match billing — but checkout does not collect billing
#                name or address today.
KNOWN_SEMANTICS = {
    "TransactionAmt",   # amount
    "ProductCD",        # merchant category
    "card4",            # brand: visa / mastercard / amex / discover
    "card6",            # funding: debit / credit
    "P_emaildomain",    # purchaser email domain
    "R_emaildomain",    # recipient email domain
    "addr1", "addr2",   # billing region
    "DeviceType", "DeviceInfo",
}


def servable_feature_columns(df: pd.DataFrame) -> list[str]:
    """Only what the charge path can actually produce for a new charge."""
    return [c for c in wide_feature_columns(df, include_v=False)
            if c in KNOWN_SEMANTICS]


def wide_feature_columns(df: pd.DataFrame, include_v: bool) -> list[str]:
    """Every usable column, not just the hand-picked ones.

    Restricting to 18 features threw away most of the dataset. These groups are
    all conceptually computable by a real gateway, even though Vesta does not
    document their exact definitions:

      C1-C14   counting features — how many addresses, cards, emails are
               associated with this entity. Our Redis velocity counters are the
               same shape.
      D1-D15   timedeltas — days since first/last seen for various entities.
      M1-M9    match flags — does the name on the card match the billing name,
               does the address match. A gateway computes these directly.
      id_01-38 device and identity signals from Vesta's SDK. PayFlow's own
               device fingerprint and behavioral collector occupy this role.

      V1-V339  Vesta's proprietary engineered features. Genuinely opaque and
               genuinely unreproducible. Included only in the benchmark model,
               to measure the ceiling — never in the servable one.
    """
    skip = {"TransactionID", "isFraud", "TransactionDT"}
    cols = []
    for c in df.columns:
        if c in skip or c.startswith("_"):
            continue
        if c.startswith("V") and not include_v:
            continue
        cols.append(c)
    return cols


def build_wide(df: pd.DataFrame, maps: dict | None = None, include_v: bool = False,
               servable_only: bool = False):
    """Wide feature matrix: engineered velocity features plus raw columns.

    Categoricals are label-encoded against maps fitted on train only; numerics
    pass through, with NaN left for XGBoost to route natively.
    """
    engineered, maps = build_features(df, maps)

    source_cols = (servable_feature_columns(df) if servable_only
                   else wide_feature_columns(df, include_v))
    raw = df[source_cols].copy()
    for col in raw.columns:
        if raw[col].dtype == object:
            key = f"_raw_{col}"
            if key not in maps:
                maps[key] = {v: i for i, v in enumerate(sorted(raw[col].dropna().astype(str).unique()))}
            raw[col] = raw[col].astype(str).map(maps[key]).fillna(-1)
        raw[col] = pd.to_numeric(raw[col], errors="coerce").astype(np.float32)

    raw.index = engineered.index
    # Engineered columns win on name collision — ours are leak-checked.
    raw = raw.drop(columns=[c for c in raw.columns if c in engineered.columns], errors="ignore")
    # Also drop raw columns already label-encoded into an engineered feature.
    # Keeping both ships the same signal twice under two names, which does not
    # hurt the model but makes the serving contract ambiguous — and an
    # ambiguous contract is how the train/serve mismatch keeps happening.
    raw = raw.drop(columns=[c for c in _SOURCE.values() if c in raw.columns] +
                           [c for c in ("TransactionAmt",) if c in raw.columns],
                   errors="ignore")
    return pd.concat([engineered, raw], axis=1), maps


def load(raw_dir, wide: bool = False, include_v: bool = False) -> pd.DataFrame:
    """Load transactions joined with the identity table.

    The identity join is a LEFT join on purpose: only ~24% of transactions have
    identity data, and dropping the rest would discard three quarters of the
    dataset — and bias it, since whether identity was captured is itself
    correlated with the channel.
    """
    from pathlib import Path

    raw_dir = Path(raw_dir)
    txn_path = raw_dir / "train_transaction.csv"

    if not wide:
        cols = ["TransactionID", "isFraud", "TransactionDT", "TransactionAmt",
                "ProductCD", "card1", "card4", "card6", "addr1", "P_emaildomain"]
        txn = pd.read_csv(txn_path, usecols=cols)
        ident = pd.read_csv(raw_dir / "train_identity.csv",
                            usecols=["TransactionID", "DeviceType"])
        return txn.merge(ident, on="TransactionID", how="left")

    # Wide mode. Reading 434 float64 columns needs ~4GB and OOMs during pandas
    # block consolidation, so numerics are forced to float32 up front — half
    # the memory, and more precision than a fraud score needs.
    header = pd.read_csv(txn_path, nrows=0)
    keep = [c for c in header.columns if include_v or not c.startswith("V")]
    dtypes = {c: np.float32 for c in keep
              if c.startswith(("V", "C", "D", "id_")) or c in ("TransactionAmt", "addr1", "addr2", "dist1", "dist2")}

    txn = pd.read_csv(txn_path, usecols=keep, dtype=dtypes)
    ident = pd.read_csv(raw_dir / "train_identity.csv")
    for c in ident.columns:
        if c.startswith("id_") and ident[c].dtype != object:
            ident[c] = ident[c].astype(np.float32)
    return txn.merge(ident, on="TransactionID", how="left")
