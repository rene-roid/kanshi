FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

WORKDIR /opt/kanshi

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY app ./app
COPY web ./web

EXPOSE 8100

# Probe the address uvicorn actually bound to — with network_mode: host that
# may be the Tailscale IP rather than loopback.
HEALTHCHECK --interval=60s --timeout=5s --start-period=15s --retries=3 \
  CMD python -c "import os,urllib.request,sys; \
h=os.environ.get('KANSHI_HOST','127.0.0.1'); h='127.0.0.1' if h=='0.0.0.0' else h; \
sys.exit(0 if urllib.request.urlopen('http://%s:%s/healthz' % (h, os.environ.get('KANSHI_PORT','8100')), timeout=4).status==200 else 1)"

CMD ["sh", "-c", "exec python -m uvicorn app.main:app \
  --host ${KANSHI_HOST:-0.0.0.0} --port ${KANSHI_PORT:-8100} \
  --no-access-log --timeout-keep-alive 65"]
