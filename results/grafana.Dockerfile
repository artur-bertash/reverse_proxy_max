# grafana with the same dashboard as the live one on the loadgen. build from the repo root:
#   docker build -f results/grafana.Dockerfile .
FROM grafana/grafana:12.1.0
COPY infra/roles/monitoring/files/grafana/provisioning /etc/grafana/provisioning
COPY infra/roles/monitoring/files/grafana/dashboards /var/lib/grafana/dashboards

ENV PROMETHEUS_URL=http://prometheus:9090 \
    GF_SERVER_ENABLE_GZIP=true \
    GF_AUTH_ANONYMOUS_ENABLED=true \
    GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer \
    GF_AUTH_DISABLE_LOGIN_FORM=true \
    GF_AUTH_BASIC_ENABLED=false \
    GF_USERS_ALLOW_SIGN_UP=false \
    GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH=/var/lib/grafana/dashboards/proxy-capacity.json \
    GF_ANALYTICS_REPORTING_ENABLED=false \
    GF_ANALYTICS_CHECK_FOR_UPDATES=false \
    GF_NEWS_NEWS_FEED_ENABLED=false

# login is off anyway, but dont leave admin/admin sitting on the internet: random password every start
ENTRYPOINT ["/bin/sh", "-c", "GF_SECURITY_ADMIN_PASSWORD=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \\n') exec /run.sh"]
