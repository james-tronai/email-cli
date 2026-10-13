# hermes-email

Connects an existing [Hermes](https://github.com/NousResearch/hermes-agent) agent on Kubernetes to its mailboxes with [email-cli](https://github.com/james-tronai/email-cli). Your Hermes Deployment stays yours. This chart only adds things that can live next to it:

| what | does |
| --- | --- |
| ConfigMap | `policy.toml` (what the agent may do; read-only to it), the skill notes, an optional cron prompt |
| watch Deployment | `email watch` in its own pod: IMAP IDLE or a 60 s poll per account, then a POST to the Hermes webhook when mail arrives. Metadata only; never sends or changes mail |
| init containers | on every start, on the Hermes volume: the `email` binary for the agent (`<home>/.local/bin/email`, a wrapper that pins the policy and directories), the skill (`skills/comms/email/SKILL.md`), the webhook route script (`scripts/email-summary.py`), and optionally the triage cron job |
| NetworkPolicies | watch: no ingress; egress to DNS, the Hermes webhook and the public internet. Optionally an ingress rule on the Hermes pod for the webhook |

Everything comes from the image of the same release, so the skill, script and binary always match.

## Install

```sh
helm install alice-email oci://ghcr.io/james-tronai/charts/hermes-email --version 0.10.2 -n hermes -f values.yaml
```

Or, with Argo CD, use the Git repository: `repoURL: https://github.com/james-tronai/email-cli.git`, `path: charts/hermes-email`, `targetRevision: v0.10.2`.

[examples/values.yaml](examples/values.yaml) is a complete example. The required values are:
- `hermes.pvc`: the PVC that holds HERMES_HOME;
- `hermes.webhookURL`: where watch posts.

Also set:
- `hermes.podSelector`: the Hermes pod's labels. Watch is then scheduled onto the same node, which a ReadWriteOnce volume (local-path) needs.
- `hermes.webhookSecret`: the Secret holding `WEBHOOK_SECRET`.

## What your Hermes Deployment needs

1. **Policy**: mount this chart's ConfigMap read-only at `/etc/email` (`paths.policyMount`) in the Hermes container. Put `<home>/.local/bin` on its PATH.
2. **Webhook**: `WEBHOOK_ENABLED=true` and `WEBHOOK_SECRET` in its environment.
3. **config.yaml**: the route and skills settings. Run `email hermes config` to print them. The route must be a `cron_job` route, because an agent route treats the run as a human message and does not honour `[SILENT]`.
4. **Network**: if Hermes has an ingress NetworkPolicy, it must let the watch pods reach port 8644. Set `networkPolicy.hermesIngress: true` to have the chart add that rule. Leave it off when Hermes has no ingress policy: the first ingress policy on a pod blocks every other connection to it.
5. **Accounts**: log in once from the Hermes container: `email auth login …` ([docs](https://github.com/james-tronai/email-cli#readme)). Tokens stay on the volume.

## The triage cron job

`seedCron.enabled` runs `email hermes seed-cron` in an init container on the Hermes image. That image has the `hermes` CLI. The step does two things:
- creates the `email-triage` job, or updates it to match `seedCron.*`;
- leaves a job that already matches untouched, and keeps a paused job paused.

If the step fails, watch starts anyway.

The webhook route's `cron_job` names the job, so new mail runs it at once with the new messages as context. The schedule (default every 2h) is only the fallback.

## Upgrading

Bump the chart version. The init containers replace the binary, skill and script atomically on the next start. Watch keeps its state on the volume (`<state dir>/watch`), so it neither repeats nor loses a notification.
