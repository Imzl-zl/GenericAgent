# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
FROM python:3.11-slim-bookworm@sha256:b18992999dbe963a45a8a4da40ac2b1975be1a776d939d098c647482bcad5cba
ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    HOME=/home/ga-bot \
    PYTHONPATH=/opt/ga/legacy

RUN groupadd --system --gid 10003 ga-delivery \
    && groupadd --system --gid 10002 ga-bot \
    && useradd --system --uid 10002 --gid 10002 --groups ga-delivery --create-home --home-dir /home/ga-bot ga-bot \
    && install -d -o 10002 -g 10002 -m 0700 /home/ga-bot/.wxbot \
    && install -d -o 10002 -g 10003 -m 2770 /var/lib/ga/bot-media \
    && install -d -o 10002 -g 10003 -m 2770 /var/lib/ga/delivery-spool \
    && pip install --no-cache-dir 'requests>=2.28' 'pycryptodome>=3.19' 'qrcode>=7.4' 'pillow>=9.0' \
    && pip install --no-cache-dir 'qq-botpy>=1.0' 'lark-oapi>=1.7' 'wecom-aibot-sdk>=1.0' 'dingtalk-stream>=0.20'

COPY tenant_platform/bot_poller/poller_server.py /opt/ga/legacy/tenant_platform/bot_poller/poller_server.py
COPY frontends/wxbot_client.py frontends/wxbot_media.py /opt/ga/legacy/frontends/
RUN chmod -R a-w /opt/ga/legacy

USER 10002:10002
WORKDIR /opt/ga/legacy
EXPOSE 8090
ENTRYPOINT ["/bin/sh", "-c", "umask 0027; exec python3 /opt/ga/legacy/tenant_platform/bot_poller/poller_server.py \"$@\"", "--"]
