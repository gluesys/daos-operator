<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# Contributing

Thanks for looking. Read the first section before you spend time on a change —
the repository is mirrored, and that changes how a contribution reaches us.

## Where this repository lives

The upstream is a **GitLab instance inside Gluesys**. GitHub
(`gluesys/daos-operator`) is a **push mirror** of it, refreshed about every five
minutes.

That has one consequence you cannot work around: **a commit pushed to GitHub
will be overwritten by the next mirror run.** A pull request opened against the
GitHub repository cannot be merged there either. This is not a policy about
outside contributions; it is how the mirror is configured.

So:

- **Bug report, question, or a design proposal** — open a GitHub issue. That
  works normally and is the right first step for anything larger than a typo.
- **A patch** — open a GitHub pull request anyway. We cannot press merge on it,
  but we can read it, discuss it in place, and carry the commits to the internal
  upstream with your authorship kept (`git am` of your patches, or a cherry-pick
  that preserves `Author:`). We will tell you in the PR when it has landed, and
  the mirror will bring it back to GitHub within a few minutes. Then the PR gets
  closed as merged-via-mirror, not rejected.
- If your change is large, please open the issue first. Being asked to restructure
  a big patch after the fact is a waste of your evening.

## Language

Code, comments, API documentation, commit messages and anything a user reads are
**English**. The repository started inside Gluesys and some of its history and
internal notes are Korean; files that are deliberately Korean carry a `.ko.md`
suffix or are marked *(Korean)* where the README links them. Those are working
records, not translations waiting to happen — there is no English counterpart.

Please do not rewrite old Korean commit messages: the GitHub mirror and the
published chart provenance both depend on the existing commit ids.

## Before you send it

Everything the CI checks before it touches hardware runs locally. You need Go
1.26 and Helm; `make` fetches `controller-gen` and `setup-envtest` on demand.

```bash
go build ./...
go vet ./...
gofmt -l api/ internal/ cmd/          # must print nothing

make manifests generate               # regenerates CRDs and deepcopy
git diff --exit-code -- config/crd api/

make helm-sync                        # copies CRDs, RBAC and the dashboard into the chart
git diff --exit-code -- charts/

make test                             # unit tests + envtest
helm lint charts/daos-operator
```

Two generated files have their own commands, and CI fails if they drift from
their source:

- `charts/daos-operator/values.schema.json` — `hack/gen-values-schema.py`, with
  `--check` to verify. It carries each key's **comment** from `values.yaml` as the
  schema `description`, and Artifact Hub renders those. If you edit a comment in
  `values.yaml`, regenerate.
- `charts/daos-operator/templates/clusterrole.yaml` — `make helm-sync`, from
  `config/rbac/role.yaml`.

Plus the license check: every `.go`, `.py`, `.sh`, `.c`, `.cpp`, `.h` and
`Dockerfile*` must carry

```
SPDX-License-Identifier: Apache-2.0
```

in its first five lines, with your own copyright line below it if the file is
substantially yours.

## What CI cannot check, and what we ask instead

The end-to-end tests drive a real DAOS system: NVMe handed to SPDK, hugepages, a
fabric, several nodes. They run on a dedicated cluster from the internal CI, on
two lanes that are reset from a snapshot before every run. There is no way to run
them from a fork.

If your change touches a path only the cluster exercises — engine lifecycle,
formatting, rank membership, upgrade — say so in the PR and describe what you did
run. We will run the tiers against the testbed before landing it. **You are not
expected to own hardware to contribute.**

`test/e2e/` is readable without the hardware, and the profiles under
`charts/daos-operator/ci/` show what each configuration looks like.

## Claims about performance

This project has retracted several of its own conclusions — a transport was
blamed for data corruption that turned out to be two server ranks formatting the
same drives, and a "hardware ceiling" turned out to be one transport's loss. The
retractions are in the docs on purpose.

So if a change comes with a number, please give the conditions with it: the
storage class and tier layout, the fabric and provider, the object class and
chunk size, how many concurrent requests, and how many runs the number came from.
A measured range beats a single best result. If a claim rests on one trial, say
that too.

## Commits

- One logical change per commit. The message should say **why**, since the diff
  already says what.
- Sign off your commits (`git commit -s`), which adds a `Signed-off-by:` line.
  It is the [Developer Certificate of Origin](https://developercertificate.org/):
  you are stating you have the right to submit the work under this project's
  license. There is no CLA.
- Use a real name and a working email in `Author:`. Some of our own early history
  has the email in the name field; do not copy that.
- Contributions are accepted under Apache-2.0 (see `LICENSE` §5).

## Layout

| Path | What it is |
|---|---|
| `api/v1alpha1/` | the CRD Go types — `DaosSystem`, `DaosPool`, `DaosContainer`, `S3Service`. This is the public API surface |
| `internal/controller/` | one reconciler per kind, plus the engine, format, rank and upgrade paths |
| `internal/dmg/`, `internal/certs/`, `internal/discovery/`, `internal/render/` | the `dmg` wrapper, certificate handling, node facts, manifest rendering |
| `cmd/` | the manager (`cmd/main.go`), the `hostprep` DaemonSet binary, the `kubectl-daos` plugin |
| `charts/daos-operator/` | the Helm chart, its values schema and the `ci/` profiles |
| `config/` | kubebuilder manifests — CRD bases, RBAC, the Grafana dashboard |
| `test/e2e/` | the end-to-end suite the CI tiers run |
| `doc/adr/` | the decisions behind the deployment, device-binding and upgrade models *(Korean)* |
| `hack/` | generators and CI cluster scripts |
| `images/hostprep/` | the hostprep image build |

The storage classes are not equally mature: `nvme` is what the tiers exercise
most, while `kdev` and `file` exist for testing against upstream defaults and are
not meant for production — `charts/daos-operator/README.md` says so where it
describes them.

## Contact

Open an issue for anything public. For a security report, do not use the issue
tracker — see `SECURITY.md`.
