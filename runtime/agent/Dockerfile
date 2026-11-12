# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM python:3.12-slim@sha256:a6e34c598f2467ed0e9a8d349809fcd8b5c603269512df273a0bb1784edc11b1

WORKDIR /app

RUN sed -i 's|http://deb.debian.org|https://deb.debian.org|g' /etc/apt/sources.list.d/debian.sources \
    && apt-get update --error-on=any \
    && apt-get upgrade -y --no-install-recommends \
    && rm -rf /var/lib/apt/lists/*

# Install dependencies first for better caching
COPY runtime/agent/requirements.txt runtime/agent/requirements-autogen.txt ./
ARG KOGNITION_RUNTIME_PROFILE=native
ARG PIP_INDEX_URL=https://pypi.org/simple
RUN case "$PIP_INDEX_URL" in https://*) ;; *) echo "package feed must use HTTPS" >&2; exit 1 ;; esac && \
	case "$KOGNITION_RUNTIME_PROFILE" in \
		native) pip install --no-cache-dir -r requirements.txt ;; \
		autogen) pip install --no-cache-dir -r requirements-autogen.txt ;; \
		*) echo "unsupported runtime dependency profile" >&2; exit 1 ;; \
	esac && \
	python -m pip uninstall --yes pip

COPY LICENSE.TXT NOTICE PRIVACY /usr/share/kognition/
COPY tools/distribution_notices.py /tmp/distribution_notices.py
ARG KOGNITION_NOTICE_STRICT=false
RUN python /tmp/distribution_notices.py python --include-interpreter --output /usr/share/kognition/python --strict "$KOGNITION_NOTICE_STRICT" \
    && rm /tmp/distribution_notices.py

# Copy application code
COPY runtime/agent/*.py .

# Non-root user for security
RUN adduser --disabled-password --gecos "" agentuser
USER agentuser

EXPOSE 8080

ENV KOGNITION_PORT=8080
ENV PYTHONUNBUFFERED=1

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
	CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/healthz', timeout=2)"]

ENTRYPOINT ["python", "main.py"]
