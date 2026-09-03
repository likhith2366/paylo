# Fraud scoring service (§14.3).
FROM python:3.11-slim

WORKDIR /app

# Dependencies first so this layer caches across source and model changes.
COPY ml/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY ml/serving/ ./ml/serving/
COPY ml/features.py ./ml/

# The trained model. Baked into the image rather than mounted, so a running
# container's scoring behaviour is pinned to its tag — "which model produced
# this decline?" must be answerable months later during a dispute (§14.5).
COPY ml/artifacts/ ./ml/artifacts/

# Non-root: this service is reachable from the payments path and has no reason
# to run privileged.
RUN useradd --create-home --uid 10001 scorer && chown -R scorer /app
USER scorer

EXPOSE 8000

# One worker per container, scaled by replica count rather than by threads.
# The model is held in memory per worker, so extra workers multiply memory for
# no throughput gain on a task this short.
CMD ["uvicorn", "ml.serving.app:app", "--host", "0.0.0.0", "--port", "8000", "--workers", "1"]
