# prometheus with every saved run baked in. build from the repo root (northflank does the same):
#   docker build -f results/prometheus.Dockerfile .
FROM prom/prometheus:v3.5.0
COPY results /runs

# merge all snapshots into one db at start (so the files in the image never change), scrape nothing.
# timeout + max samples so one heavy query from a visitor cant hog it
ENTRYPOINT ["/bin/sh", "-c", "mkdir -p /tmp/db && for s in /runs/*/prometheus; do cp -r \"$s\"/. /tmp/db/; done && echo 'global: {}' > /tmp/prometheus.yml && exec /bin/prometheus --config.file=/tmp/prometheus.yml --storage.tsdb.path=/tmp/db --storage.tsdb.retention.time=100y --web.listen-address=:9090 --query.timeout=20s --query.max-samples=5000000"]
