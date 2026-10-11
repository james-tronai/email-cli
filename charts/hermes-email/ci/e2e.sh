#!/bin/sh
# End-to-end test of the chart in a kind cluster (CI job "chart"): a stand-in Hermes
# pod with a webhook, a fake mailbox on the shared volume, the chart, then one new
# message must reach the webhook with the right token, and the route script must
# turn it into prompt variables. Needs the image email-cli:ci loaded into kind.
set -eu
ns=hermes
k() { kubectl -n "$ns" "$@"; }
chart="$(dirname "$0")/.."

kubectl create namespace "$ns"
k apply -f - <<'YAML'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: data}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 100Mi}}
---
apiVersion: v1
kind: Secret
metadata: {name: webhook}
stringData: {WEBHOOK_SECRET: ci-secret}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: fake-hermes}
data:
  server.py: |
    import http.server, os
    class H(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(200); self.end_headers(); self.wfile.write(b"ok")
        def do_POST(self):
            body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
            ok = self.headers.get("X-Gitlab-Token") == os.environ["WEBHOOK_SECRET"]
            if ok:
                with open("/opt/data/received.jsonl", "ab") as f:
                    f.write(body + b"\n")
            self.send_response(202 if ok else 401); self.end_headers()
    http.server.HTTPServer(("", 8644), H).serve_forever()
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: fake-hermes}
spec:
  selector: {matchLabels: {app: hermes}}
  template:
    metadata: {labels: {app: hermes}}
    spec:
      securityContext: {runAsUser: 10000, runAsGroup: 10000, fsGroup: 10000}
      containers:
        - name: hermes
          image: python:3.12-alpine
          command: [python, -u, /srv/server.py]
          env:
            - name: WEBHOOK_SECRET
              valueFrom: {secretKeyRef: {name: webhook, key: WEBHOOK_SECRET}}
          ports: [{containerPort: 8644}]
          volumeMounts:
            - {name: data, mountPath: /opt/data}
            - {name: srv, mountPath: /srv}
      volumes:
        - {name: data, persistentVolumeClaim: {claimName: data}}
        - {name: srv, configMap: {name: fake-hermes}}
---
apiVersion: v1
kind: Service
metadata: {name: hermes-webhook}
spec:
  selector: {app: hermes}
  ports: [{port: 8644}]
---
apiVersion: batch/v1
kind: Job
metadata: {name: login}
spec:
  template:
    spec:
      restartPolicy: Never
      securityContext: {runAsUser: 10000, runAsGroup: 10000, fsGroup: 10000}
      containers:
        - name: login
          image: email-cli:ci
          imagePullPolicy: Never
          args: [auth, login, -a, demo, --provider, fake, --mode, draft, -q]
          env: [{name: EMAIL_CONFIG_DIR, value: /opt/data/.config/email}]
          volumeMounts: [{name: data, mountPath: /opt/data}]
      volumes: [{name: data, persistentVolumeClaim: {claimName: data}}]
YAML
k rollout status deploy/fake-hermes --timeout=180s
k wait job/login --for=condition=complete --timeout=120s

cat >/tmp/values-ci.yaml <<'YAML'
image: {repository: email-cli, tag: ci, pullPolicy: Never}
hermes:
  pvc: data
  webhookURL: http://hermes-webhook:8644/webhooks/email
  podSelector: {app: hermes}
  webhookSecret: {name: webhook}
watch: {debounce: 2s, pollInterval: 5s}
networkPolicy: {hermesIngress: true}
skillNotes: "CI notes: the only account is `demo`."
summary: {timezone: Australia/Sydney, accountLabels: {demo: CI mailbox}}
policy: |
  [default]
  mode = "draft"
  [watch]
  only_unread = false
YAML
helm install mail "$chart" -n "$ns" -f /tmp/values-ci.yaml --wait --timeout 3m
watch=deploy/mail-hermes-email-watch

echo "== files on the volume"
k exec deploy/fake-hermes -- grep -q "CI notes" /opt/data/skills/comms/email/SKILL.md
k exec deploy/fake-hermes -- test -f /opt/data/scripts/email-summary.py
k exec deploy/fake-hermes -- grep -q Australia/Sydney /opt/data/scripts/email-summary.json
k exec deploy/fake-hermes -- /opt/data/.local/bin/email version
k exec deploy/fake-hermes -- grep -q "EMAIL_POLICY_FILE='/etc/email/policy.toml'" /opt/data/.local/bin/email

echo "== watch health"
k exec "$watch" -- /email watch healthz --health-addr 127.0.0.1:8645

echo "== new mail reaches the webhook"
id="$(k exec "$watch" -- /email -a demo draft create --to demo@example.test --subject "chart e2e" --body hi --jq .id)"
k exec "$watch" -- /email -a demo move "$id" --to inbox -q
i=0
until k exec deploy/fake-hermes -- grep -q "chart e2e" /opt/data/received.jsonl 2>/dev/null; do
	i=$((i + 1))
	if [ "$i" -gt 30 ]; then
		k logs "$watch" --tail=50
		echo "no notification within 60s" >&2
		exit 1
	fi
	sleep 2
done
k exec deploy/fake-hermes -- sh -c 'tail -n 1 /opt/data/received.jsonl | python3 -I /opt/data/scripts/email-summary.py' >/tmp/route.json
cat /tmp/route.json
grep -q '"account_label": "CI mailbox"' /tmp/route.json
grep -q 'subject: chart e2e' /tmp/route.json
k logs "$watch" | grep -q '"event":"notify_ok"'
echo "chart e2e ok"
