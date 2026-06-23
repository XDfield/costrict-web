# Helm Deploy Refactor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement this plan task-by-task, or `superpowers:subagent-driven-development` if you prefer fresh subagents per chart.

**Goal:** Refactor all Helm charts under `deploy/charts/` so every chart defaults to the `costrict-web` namespace, names resources with the Helm release name, uses the cluster `default` ServiceAccount, and runs database migrations from the API server startup instead of a Helm hook Job.

**Architecture:** Keep the existing chart-per-service layout. Centralise naming/namespace helpers in each `_helpers.tpl`, delete `serviceaccount.yaml` templates, and remove the `api/templates/migrate-job.yaml` hook. A small change in `server/cmd/api/main.go` runs the bundled `./migrate` binary when `RUN_MIGRATIONS=true`, making migration independent of Helm/K8s Job lifecycle.

**Tech Stack:** Helm 3, Go, Kubernetes.

---

## Task 1: Prepare the branch

**Files:** none created; branch changes.

- [ ] **Step 1: Confirm you are on the feature branch**

  Run:

  ```bash
  git status
  ```

  Expected: branch is `worktree-feat+deploy-refactor` and the working tree is clean.

---

## Task 2: Add namespace default and helper to every chart

**Files:**
- Modify: `deploy/charts/{api,gateway,portal,postgres,proxy,worker}/values.yaml`
- Modify: `deploy/charts/{api,gateway,portal,postgres,proxy,worker}/templates/_helpers.tpl`
- Modify: every namespaced template YAML under `deploy/charts/{api,gateway,portal,postgres,proxy,worker}/templates/` except `storageclass.yaml`

- [ ] **Step 1: Add `namespace` value to each `values.yaml`**

  Insert after the first comment line, e.g. for `deploy/charts/api/values.yaml`:

  ```yaml
  # Default values for costrict-web-api

  namespace: costrict-web
  ```

  Repeat for `gateway`, `portal`, `postgres`, `proxy`, and `worker` values files.

- [ ] **Step 2: Add a `namespace` helper to each `_helpers.tpl`**

  For `deploy/charts/api/templates/_helpers.tpl` add:

  ```gotemplate
  {{/*
  Namespace to use for all namespaced resources.
  */}}
  {{- define "api.namespace" -}}
  {{- default "costrict-web" .Values.namespace }}
  {{- end }}
  ```

  Replace `api` with the chart key (`gateway`, `portal`, `postgres`, `proxy`, `worker`) in each chart.

- [ ] **Step 3: Render namespace in every namespaced resource**

  In each namespaced template (`deployment.yaml`, `daemonset.yaml`, `service.yaml`, `configmap.yaml`, `secret.yaml`, `pvc.yaml`, `serviceaccount.yaml`), add the namespace line directly under `metadata.name`, e.g.:

  ```yaml
  metadata:
    name: {{ include "api.fullname" . }}
    namespace: {{ include "api.namespace" . }}
    labels:
      {{- include "api.labels" . | nindent 4 }}
  ```

  Do **not** add `namespace` to `storageclass.yaml` because `StorageClass` is cluster-scoped.

---

## Task 3: Switch resource names to the Helm release name

**Files:**
- Modify: `deploy/charts/{api,gateway,portal,postgres,proxy,worker}/templates/_helpers.tpl`

- [ ] **Step 1: Replace the `fullname` helper in every chart**

  For `deploy/charts/api/templates/_helpers.tpl`, change the `api.fullname` definition to:

  ```gotemplate
  {{- define "api.fullname" -}}
  {{- if .Values.fullnameOverride }}
  {{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
  {{- else }}
  {{- .Release.Name | trunc 63 | trimSuffix "-" }}
  {{- end }}
  {{- end }}
  ```

  Replace `api` with the chart key for the other five charts.

  Leave the `name` helper (used for labels) and `nameOverride` unchanged.

---

## Task 4: Remove ServiceAccount creation and use the default SA

**Files:**
- Delete: `deploy/charts/{api,gateway,portal,postgres,proxy,worker}/templates/serviceaccount.yaml`
- Modify: `deploy/charts/{api,gateway,portal,postgres,proxy,worker}/templates/_helpers.tpl`
- Modify: `deploy/charts/{api,gateway,portal,postgres,proxy,worker}/values.yaml`
- Modify: every `deployment.yaml`/`daemonset.yaml` workload template

- [ ] **Step 1: Delete all `serviceaccount.yaml` templates**

  ```bash
  rm deploy/charts/*/templates/serviceaccount.yaml
  ```

- [ ] **Step 2: Remove the `serviceAccountName` helper from each `_helpers.tpl`**

  Delete the entire `Create the name of the service account to use` block (the `*.serviceAccountName` define) from each chart's `_helpers.tpl`.

- [ ] **Step 3: Set `serviceAccountName: default` in workload specs**

  In every `deployment.yaml` and `daemonset.yaml`, replace:

  ```yaml
  serviceAccountName: {{ include "api.serviceAccountName" . }}
  ```

  with:

  ```yaml
  serviceAccountName: default
  ```

  using the appropriate chart key for that chart.

- [ ] **Step 4: Remove the `serviceAccount:` block from each `values.yaml`**

  Remove:

  ```yaml
  serviceAccount:
    create: true
    annotations: {}
    name: ""
  ```

  from all six values files.

---

## Task 5: Replace the Helm migration Job with API startup migration

**Files:**
- Delete: `deploy/charts/api/templates/migrate-job.yaml`
- Modify: `deploy/charts/api/values.yaml`
- Modify: `deploy/charts/api/templates/deployment.yaml`
- Modify: `server/cmd/api/main.go`

- [ ] **Step 1: Delete the Helm hook Job template**

  ```bash
  rm deploy/charts/api/templates/migrate-job.yaml
  ```

- [ ] **Step 2: Remove the `migrate` value block from `deploy/charts/api/values.yaml`**

  Delete:

  ```yaml
  # Database migration job
  migrate:
    enabled: false
  ```

- [ ] **Step 3: Add `runMigrations` config to `deploy/charts/api/values.yaml`**

  Add under the existing `config:` block:

  ```yaml
  # API specific configuration
  config:
    port: "8080"
    logLevel: "info"
    runMigrations: true
  ```

- [ ] **Step 4: Expose `RUN_MIGRATIONS` in the API Deployment**

  In `deploy/charts/api/templates/deployment.yaml`, after the `LOG_LEVEL` env entry add:

  ```yaml
            - name: LOG_LEVEL
              value: {{ .Values.config.logLevel | quote }}
            - name: RUN_MIGRATIONS
              value: {{ .Values.config.runMigrations | quote }}
  ```

- [ ] **Step 5: Run migrations from the API server startup**

  In `server/cmd/api/main.go`, add `os/exec` to the imports and insert the migration runner right after logger init and before `cfg := config.Load()`:

  ```go
  // Run database migrations from the same image before starting the server.
  // The migrate binary is bundled in the production image; in local dev it may
  // be absent, in which case we warn and continue.
  if os.Getenv("RUN_MIGRATIONS") != "false" {
      if _, err := os.Stat("./migrate"); err == nil {
          log.Println("Running database migrations...")
          migrateCmd := exec.Command("./migrate")
          migrateCmd.Stdout = os.Stdout
          migrateCmd.Stderr = os.Stderr
          if err := migrateCmd.Run(); err != nil {
              log.Fatalf("Database migration failed: %v", err)
          }
          log.Println("Database migrations completed")
      } else {
          log.Printf("Migration binary not found, skipping: %v", err)
      }
  }
  ```

  Ensure `os` is already imported (it is) and `log` is imported (it is).

---

## Task 6: Validate all charts render correctly

**Files:** none modified.

- [ ] **Step 1: Template each chart with the new defaults**

  Run:

  ```bash
  for chart in api gateway portal postgres proxy worker; do
    echo "--- $chart ---"
    helm template "rel-$chart" "deploy/charts/$chart" --debug > /dev/null
  done
  ```

  Expected: all six commands exit 0.

- [ ] **Step 2: Inspect the rendered API manifest for the key changes**

  Run:

  ```bash
  helm template rel-api deploy/charts/api
  ```

  Verify:
  - `metadata.namespace: costrict-web` appears on every namespaced resource.
  - Resource names are `rel-api` (not `rel-api-costrict-web-api`).
  - `serviceAccountName: default` in the Deployment.
  - No `Job` manifest is rendered.
  - `RUN_MIGRATIONS: "true"` appears in the container env.

---

## Task 7: Commit and open a Pull Request

**Files:** all changed files.

- [ ] **Step 1: Stage the changes**

  ```bash
  git add -A
  ```

- [ ] **Step 2: Commit with a descriptive message**

  ```bash
  git commit -m "refactor(deploy): namespace/release-name/default SA and API startup migrations

  - All charts default namespace to costrict-web and include explicit namespace metadata.
  - Resource names use the Helm release name only.
  - Remove per-chart ServiceAccount templates; workloads use the default SA.
  - Remove the Helm hook migration Job; migrations now run via the API server startup.

  Co-Authored-By: Claude <noreply@anthropic.com>"
  ```

- [ ] **Step 3: Push the feature branch**

  ```bash
  git push origin worktree-feat+deploy-refactor
  ```

- [ ] **Step 4: Open a Pull Request targeting `main`**

  Use the `gh` CLI:

  ```bash
  gh pr create --base main --title "refactor(deploy): helm namespace, release-name, default SA and API startup migrations" --body "...

  🤖 Generated with [Claude Code](https://claude.com/claude-code)"
  ```

  Do **not** merge the PR without explicit user approval.

---

## Self-review checklist

- [ ] Every namespaced resource has `namespace: costrict-web` by default.
- [ ] Resource names are driven by `.Release.Name`.
- [ ] No chart creates a ServiceAccount.
- [ ] The API chart no longer renders a Helm hook Job.
- [ ] `server/cmd/api/main.go` runs `./migrate` when `RUN_MIGRATIONS=true`.
- [ ] `helm template` succeeds for all six charts.
