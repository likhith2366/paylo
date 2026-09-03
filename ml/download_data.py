#!/usr/bin/env python3
"""Download the fraud training datasets from Kaggle (§14.3).

Two datasets, chosen for different jobs:

  sparkov  kartik2112/fraud-detection — 1.85M transactions with *named*,
           semantically meaningful columns (card number, amount, merchant
           category, cardholder DOB, and both cardholder and merchant
           lat/long). This is the servable one: every feature it carries can
           actually be computed from a live charge at inference time.

  ieee     ieee-fraud-detection — 590k transactions, 3.5% fraud, 434 features
           including email domain, device info, and pre-built counting
           features. Richer, and the standard public benchmark for this task.

A note on what is deliberately NOT here: the widely-used ULB
`mlg-ulb/creditcardfraud` set. Its features are anonymized PCA components
V1..V28, which cannot be reconstructed from an incoming charge — a model
trained on it scores well offline and cannot be deployed. Same for the
"balanced" 2023 variant of it.

On class imbalance: fraud really does run well under 1% of transactions, so an
imbalanced dataset is the realistic one. Rebalancing by resampling teaches the
model a base rate that does not exist in production. The imbalance is handled
in training instead, via scale_pos_weight, and measured with PR-AUC rather
than accuracy (see train.py).

Usage:
    python ml/download_data.py            # both datasets
    python ml/download_data.py --only sparkov
"""

from __future__ import annotations

import argparse
import subprocess
import sys
import zipfile
from pathlib import Path

RAW_DIR = Path(__file__).parent / "data" / "raw"

DATASETS = {
    "sparkov": {
        "kind": "dataset",
        "ref": "kartik2112/fraud-detection",
        "files": ["fraudTrain.csv", "fraudTest.csv"],
        "note": "1.85M transactions, named features, ~0.5% fraud",
    },
    "ieee": {
        "kind": "competition",
        "ref": "ieee-fraud-detection",
        "files": ["train_transaction.csv", "train_identity.csv"],
        "note": "590k transactions, 434 features, ~3.5% fraud",
    },
}


def kaggle(*args: str) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, "-m", "kaggle", *args],
        capture_output=True,
        text=True,
    )


def fetch(name: str, spec: dict) -> bool:
    print(f"\n=== {name}: {spec['ref']} ===")
    print(f"    {spec['note']}")

    target = RAW_DIR / name
    target.mkdir(parents=True, exist_ok=True)

    for filename in spec["files"]:
        csv_path = target / filename
        if csv_path.exists():
            size_mb = csv_path.stat().st_size / 1e6
            print(f"    [skip] {filename} already present ({size_mb:.0f} MB)")
            continue

        print(f"    [get ] {filename} ...")
        if spec["kind"] == "competition":
            result = kaggle("competitions", "download", "-c", spec["ref"],
                            "-f", filename, "-p", str(target))
        else:
            result = kaggle("datasets", "download", spec["ref"],
                            "-f", filename, "-p", str(target))

        if result.returncode != 0:
            stderr = (result.stderr or result.stdout).strip()
            if "403" in stderr or "Forbidden" in stderr:
                print(f"    [FAIL] 403 Forbidden.")
                print(f"           Accept the rules at "
                      f"https://www.kaggle.com/c/{spec['ref']}/rules first.")
            else:
                print(f"    [FAIL] {stderr[:400]}")
            return False

        # Kaggle serves single files zipped; some endpoints return them raw.
        for archive in target.glob("*.zip"):
            with zipfile.ZipFile(archive) as z:
                z.extractall(target)
            archive.unlink()

        if csv_path.exists():
            print(f"    [ ok ] {filename} ({csv_path.stat().st_size / 1e6:.0f} MB)")
        else:
            print(f"    [WARN] {filename} not found after extraction")
    return True


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--only", choices=sorted(DATASETS), help="fetch just one dataset")
    args = parser.parse_args()

    wanted = {args.only: DATASETS[args.only]} if args.only else DATASETS

    # Fail early with an actionable message rather than an opaque Kaggle error.
    token = Path.home() / ".kaggle" / "access_token"
    legacy = Path.home() / ".kaggle" / "kaggle.json"
    if not token.exists() and not legacy.exists():
        print("No Kaggle credentials found.", file=sys.stderr)
        print("Create a token at https://www.kaggle.com/settings (API section),", file=sys.stderr)
        print(f"then save it to {token} (new format) or {legacy} (legacy).", file=sys.stderr)
        return 1

    ok = all(fetch(name, spec) for name, spec in wanted.items())
    print(f"\nData directory: {RAW_DIR}")
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
