# Kubernetes manifests

Minimal, readable manifests that map the architecture in `docs/01` onto
Kubernetes. They are intentionally plain YAML (no Helm/Kustomize) so you can read
every line. See `docs/10-deployment.md` for the walkthrough.

Apply in order:

```bash
kubectl apply -f secret.yaml        # DATABASE_URL (demo value — replace in prod)
kubectl apply -f postgres.yaml      # a single-replica Postgres for demo/dev
kubectl apply -f migrate-job.yaml   # runs `server -migrate-only` once
kubectl apply -f server.yaml        # API + dashboard + reaper (Deployment + Service)
kubectl apply -f worker.yaml        # the worker pool (Deployment, scale freely)
```

Then port-forward the dashboard:

```bash
kubectl port-forward svc/scheduler-server 8080:8080
```

Production notes (deliberately NOT baked in, so the demo stays simple):
- Replace the demo Postgres with a managed database; put its URL in the Secret.
- Run the migrate Job as part of your deploy pipeline before rolling workers.
- Scale workers with `kubectl scale deploy/scheduler-worker --replicas=N` or an
  HPA keyed on queue depth.
