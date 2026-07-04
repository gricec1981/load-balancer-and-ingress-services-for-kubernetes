# Semantic cache sidecar. Built server-side with:
#   az acr build -r akoinfdemo8161 -t cache-service:<tag> \
#     -f cache/Dockerfile cache/
FROM python:3.11-slim

WORKDIR /app

# CPU-only torch first (keeps the image ~1GB instead of multi-GB CUDA), then
# the rest of the deps.
COPY requirements.txt .
RUN pip install --no-cache-dir torch==2.5.1 \
        --index-url https://download.pytorch.org/whl/cpu \
 && pip install --no-cache-dir -r requirements.txt

COPY cache_service.py .

# Pre-bake the embedding model so the running pod needs no internet.
RUN python -c "from sentence_transformers import SentenceTransformer; \
SentenceTransformer('all-MiniLM-L6-v2')"

ENV CACHE_THRESHOLD=0.95
EXPOSE 9200
CMD ["uvicorn", "cache_service:app", "--host", "0.0.0.0", "--port", "9200"]
