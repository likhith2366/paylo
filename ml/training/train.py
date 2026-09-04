#!/usr/bin/env python3
"""Train the fraud risk model (§14.3).

Gradient-boosted trees, not deep learning. Fraud teams use GBTs because they
are fast at inference (<10ms, which the sub-100ms risk budget demands), work
well on tabular data, and are explainable enough to justify a decline to a
customer or a regulator.

Two decisions here are worth understanding before changing anything:

CHRONOLOGICAL SPLIT, NOT RANDOM
    A random split lets the model learn from a card's future transactions when
    scoring its past ones. The reported score is then far better than
    production, and the gap only shows up after deployment. Training on the
    earliest 80% and testing on the latest 20% mirrors how the model is
    actually used: predicting tomorrow from today.

IMBALANCE IS HANDLED, NOT REMOVED
    Fraud is ~0.5% of transactions here and genuinely rare in the world.
    Resampling to 50/50 would teach the model a base rate that does not exist,
    making its probabilities useless for setting a decline threshold. Instead
    scale_pos_weight tells XGBoost to weight the rare class, and evaluation
    uses PR-AUC rather than accuracy or ROC-AUC.

    Accuracy is actively misleading at this base rate: predicting "never fraud"
    scores 99.6%. ROC-AUC also flatters imbalanced classifiers because the huge
    true-negative count suppresses the false-positive rate. Precision-recall is
    the honest curve.

Usage:
    python ml/training/train.py --dataset sparkov
    python ml/training/train.py --dataset sparkov --sample 200000   # quick run
"""

from __future__ import annotations

import argparse
import json
import sys
import time
from pathlib import Path

import numpy as np
import pandas as pd
import xgboost as xgb
from sklearn.metrics import (
    average_precision_score,
    confusion_matrix,
    precision_recall_curve,
    roc_auc_score,
)

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
import features as sparkov_features  # noqa: E402
import features_ieee  # noqa: E402

ML_DIR = Path(__file__).resolve().parent.parent
RAW_DIR = ML_DIR / "data" / "raw"
ARTIFACT_DIR = ML_DIR / "artifacts"


def load_sparkov(sample: int | None) -> pd.DataFrame:
    """Load and concatenate the Sparkov train and test files.

    The published train/test split is re-derived below as a chronological one,
    so both files are combined first — the shipped split is random with respect
    to time and would leak.
    """
    frames = []
    for name in ("fraudTrain.csv", "fraudTest.csv"):
        path = RAW_DIR / "sparkov" / name
        if not path.exists():
            raise FileNotFoundError(
                f"{path} not found. Run: python ml/download_data.py --only sparkov"
            )
        print(f"  reading {name} ...")
        frames.append(pd.read_csv(path, index_col=0))

    df = pd.concat(frames, ignore_index=True)

    if sample and sample < len(df):
        # Take the most RECENT rows rather than a random sample: a random subset
        # would scatter each card's history and destroy the velocity features.
        df = df.sort_values("trans_date_trans_time").tail(sample).reset_index(drop=True)
        print(f"  sampled the most recent {sample:,} transactions")

    return df


def chronological_split(df: pd.DataFrame, test_fraction: float = 0.2,
                        time_col: str = "trans_date_trans_time"):
    """Split on time: earliest rows train, latest rows test."""
    df = df.sort_values(time_col).reset_index(drop=True)
    cutoff = int(len(df) * (1 - test_fraction))
    boundary = df.loc[cutoff, time_col]
    print(f"  split at {boundary}: {cutoff:,} train / {len(df) - cutoff:,} test")
    return df.iloc[:cutoff].copy(), df.iloc[cutoff:].copy()


def evaluate(y_true, y_prob, label: str) -> dict:
    """Score a model the way an imbalanced problem requires."""
    positives = int(np.sum(y_true))

    # With a base rate under 1%, a small or short time slice can contain no
    # fraud at all. Every metric below is undefined in that case, so say so
    # plainly instead of crashing or — worse — reporting a number.
    if positives == 0 or positives == len(y_true):
        print(f"\n  {label}")
        print(f"    only one class present ({positives:,} fraud of {len(y_true):,})")
        print("    metrics are undefined — use a larger sample or the full dataset")
        return {
            "error": "single_class",
            "positives": positives,
            "rows": int(len(y_true)),
        }

    pr_auc = average_precision_score(y_true, y_prob)
    roc_auc = roc_auc_score(y_true, y_prob)

    precision, recall, thresholds = precision_recall_curve(y_true, y_prob)

    # The operating point that matters to a business: at a chosen recall, how
    # many good customers do we inconvenience? "We catch 70% of fraud and
    # decline 1 in 200 legitimate charges" is actionable; an AUC is not.
    operating_points = {}
    for target_recall in (0.50, 0.70, 0.90):
        # precision_recall_curve returns recall in decreasing order.
        idx = np.argmin(np.abs(recall - target_recall))
        threshold = thresholds[min(idx, len(thresholds) - 1)]
        preds = (y_prob >= threshold).astype(int)
        tn, fp, fn, tp = confusion_matrix(y_true, preds, labels=[0, 1]).ravel()

        operating_points[f"recall_{int(target_recall * 100)}"] = {
            "threshold": float(threshold),
            "precision": float(precision[idx]),
            "recall": float(recall[idx]),
            "true_positives": int(tp),
            "false_positives": int(fp),
            "false_negatives": int(fn),
            # The number a merchant actually feels.
            "legit_declined_per_10k": float(fp / max(tn + fp, 1) * 10_000),
        }

    fraud_rate = float(np.mean(y_true))
    print(f"\n  {label}")
    print(f"    fraud rate         {fraud_rate * 100:.3f}%  ({int(np.sum(y_true)):,} of {len(y_true):,})")
    print(f"    PR-AUC             {pr_auc:.4f}   <- the honest metric here")
    print(f"    ROC-AUC            {roc_auc:.4f}   (flattering; reported for comparability)")
    # A model that ignored the features entirely would score the base rate.
    print(f"    PR-AUC baseline    {fraud_rate:.4f}   (a model that guesses)")
    print(f"    lift over baseline {pr_auc / fraud_rate:.1f}x")

    for name, op in operating_points.items():
        print(
            f"    @ {name:9} threshold {op['threshold']:.4f}  "
            f"precision {op['precision']:.3f}  "
            f"caught {op['true_positives']:,}  "
            f"missed {op['false_negatives']:,}  "
            f"false alarms {op['false_positives']:,} "
            f"({op['legit_declined_per_10k']:.1f} per 10k good charges)"
        )

    return {
        "pr_auc": float(pr_auc),
        "roc_auc": float(roc_auc),
        "fraud_rate": fraud_rate,
        "baseline_pr_auc": fraud_rate,
        "lift": float(pr_auc / fraud_rate),
        "operating_points": operating_points,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--dataset", default="ieee", choices=["ieee", "sparkov"])
    parser.add_argument("--sample", type=int, help="use only the N most recent rows")
    parser.add_argument("--rounds", type=int, default=400)
    parser.add_argument("--wide", action="store_true",
                        help="use every usable column, not the hand-picked set")
    parser.add_argument("--include-v", action="store_true",
                        help="also use Vesta's opaque V1-V339 (benchmark only — NOT servable)")
    args = parser.parse_args()

    ARTIFACT_DIR.mkdir(parents=True, exist_ok=True)
    started = time.time()

    print(f"Loading {args.dataset}")
    if args.dataset == "ieee":
        df = features_ieee.load(RAW_DIR / "ieee", wide=args.wide, include_v=args.include_v)
        label, time_col = "isFraud", "TransactionDT"
        if args.wide:
            build = lambda d, m=None: features_ieee.build_wide(d, m, include_v=args.include_v)
            FEATURE_COLUMNS = None  # determined after the first build
        else:
            FEATURE_COLUMNS = features_ieee.FEATURE_COLUMNS
            build = features_ieee.build_features
    else:
        df = load_sparkov(args.sample)
        label, time_col = "is_fraud", "trans_date_trans_time"
        FEATURE_COLUMNS = sparkov_features.FEATURE_COLUMNS
        build = lambda d, m=None: sparkov_features.build_features(
            d, category_map=(m or {}).get("category_map"),
            state_map=(m or {}).get("state_map"))

    if args.sample and args.sample < len(df):
        df = df.sort_values(time_col).tail(args.sample).reset_index(drop=True)
        print(f"  sampled the most recent {args.sample:,}")
    print(f"  {len(df):,} transactions, {int(df[label].sum()):,} fraudulent "
          f"({df[label].mean() * 100:.3f}%)")

    print("\nSplitting chronologically")
    train_df, test_df = chronological_split(df, time_col=time_col)

    # Carve a validation slice off the END of train for early stopping.
    #
    # Previously early stopping evaluated on the TEST set, which leaks model
    # selection into the number being reported. It happened to cost nothing
    # here — best_iteration was 399 of 400, so it never actually fired — but it
    # is a latent bug that would silently corrupt the headline the moment the
    # model did stop early. Validation comes from the tail of train so it stays
    # chronologically before test.
    val_cut = int(len(train_df) * 0.9)
    val_df = train_df.iloc[val_cut:].copy()
    train_df = train_df.iloc[:val_cut].copy()
    print(f"  held out {len(val_df):,} rows from the end of train for early stopping")

    print("\nEngineering features")
    # Encodings are fitted on training data only and reused for the test set —
    # deriving them from the test set would leak information about it.
    X_train, encodings = build(train_df)
    y_train = train_df[label].values
    if FEATURE_COLUMNS is None:
        FEATURE_COLUMNS = list(X_train.columns)

    X_val, _ = build(val_df, encodings)
    y_val = val_df[label].values

    X_test, _ = build(test_df, encodings)
    y_test = test_df[label].values
    print(f"  {len(FEATURE_COLUMNS)} features")

    # Tell XGBoost the positive class is rare rather than rebalancing the data.
    negative, positive = int((y_train == 0).sum()), int((y_train == 1).sum())
    scale_pos_weight = negative / max(positive, 1)
    print(f"  scale_pos_weight = {scale_pos_weight:.1f}  ({negative:,} legit / {positive:,} fraud)")

    print("\nTraining")
    model = xgb.XGBClassifier(
        n_estimators=args.rounds,
        max_depth=6,
        learning_rate=0.08,
        subsample=0.8,
        colsample_bytree=0.8,
        min_child_weight=3,
        scale_pos_weight=scale_pos_weight,
        # aucpr, not auc: optimizing the metric we actually report.
        eval_metric="aucpr",
        early_stopping_rounds=40,
        tree_method="hist",
        n_jobs=-1,
        random_state=42,
    )
    # eval_set is the validation slice, never test.
    model.fit(X_train, y_train, eval_set=[(X_val, y_val)], verbose=False)
    print(f"  stopped at round {model.best_iteration} of {args.rounds}")

    print("\nEvaluating")
    train_metrics = evaluate(y_train, model.predict_proba(X_train)[:, 1], "TRAIN (in-sample)")
    test_metrics = evaluate(y_test, model.predict_proba(X_test)[:, 1], "TEST (held out, later in time)")

    importances = sorted(
        zip(FEATURE_COLUMNS, model.feature_importances_),
        key=lambda kv: kv[1], reverse=True,
    )
    print("\n  top features")
    for name, score in importances[:8]:
        print(f"    {name:24} {score:.4f}")

    # --- persist -----------------------------------------------------------
    model_path = ARTIFACT_DIR / "fraud_model.json"
    model.save_model(model_path)

    # The encodings MUST ship with the model. Regenerating them from different
    # data assigns different integers to the same category, which silently
    # corrupts every prediction rather than failing loudly.
    (ARTIFACT_DIR / "encodings.json").write_text(json.dumps(encodings, indent=2))
    (ARTIFACT_DIR / "dataset.txt").write_text(args.dataset)

    metrics = {
        "dataset": args.dataset,
        "trained_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "rows_total": int(len(df)),
        "rows_train": int(len(train_df)),
        "rows_validation": int(len(val_df)),
        "rows_test": int(len(test_df)),
        "features": FEATURE_COLUMNS,
        "best_iteration": int(model.best_iteration),
        "scale_pos_weight": float(scale_pos_weight),
        "train": train_metrics,
        "test": test_metrics,
        "feature_importance": {name: float(score) for name, score in importances},
        "training_seconds": round(time.time() - started, 1),
    }
    (ARTIFACT_DIR / "metrics.json").write_text(json.dumps(metrics, indent=2))

    print(f"\nWrote {model_path.name}, encodings.json, metrics.json to {ARTIFACT_DIR}")
    print(f"Done in {metrics['training_seconds']}s")

    if "error" in test_metrics:
        print("\nERROR: the test split contained no fraud, so nothing was measured.")
        print("Run without --sample, or with a much larger one.")
        return 1

    # A test PR-AUC near the base rate means the model learned nothing useful.
    if test_metrics["pr_auc"] <= test_metrics["fraud_rate"] * 2:
        print("\nWARNING: PR-AUC is barely above the base rate — the model is not useful.")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
