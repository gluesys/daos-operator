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

## Before you send it

The CI (`.gitlab-ci.yml`, mirrored as `.github/workflows/ci.yml`) runs three
things, and all of them run on a laptop with nothing but a Python interpreter:

```bash
python3 tests/test_serde.py
python3 tests/test_serde_v2.py
find . -path ./.git -prune -o -name '*.py' -print | xargs -n50 python3 -m py_compile
find . -path ./.git -prune -o -name '*.sh' -print | xargs -n1 bash -n
```

Plus the license check: every `.py`, `.sh` and `.c` file must carry

```
SPDX-License-Identifier: Apache-2.0
Copyright 2026 Gluesys Co., Ltd.
```

in its first five lines, with your own copyright line instead if the file is
substantially yours.

**Files under `gpudirect/patches/` must NOT carry that header.** They are diffs
against DAOS, UCX, Mercury and libfabric, so the context they quote stays under
those projects' licenses. CI fails if our header appears there. See `NOTICE`.

## What CI cannot check, and what we ask instead

Most of `tests/` needs a live DAOS cluster — a pool, a container, a running
agent — and the GPU-direct tests additionally need an H100 with a patched client
stack. There is no way to run those in CI, and we deliberately did not add a job
that skips itself when the cluster is absent: a green pipeline that checked
nothing is worse than no pipeline.

If your change touches a path that only the cluster exercises, say so in the PR
and describe what you did run. We will run the integration tests against the
testbed before landing it. You are not expected to own hardware to contribute.

## Claims about performance

This project has retracted several of its own conclusions — a transport was
blamed for data corruption that turned out to be two server ranks formatting the
same drives, and a "hardware ceiling" turned out to be one transport's loss. The
retractions are in the docs on purpose.

So if a change comes with a number, please give the conditions with it: model
and context length, the DAOS object class and chunk size, the transport, how
many concurrent requests, and how many runs the number came from. A measured
range beats a single best result. If a claim rests on one trial, say that too.

## Commits

- One logical change per commit. The message should say **why**, since the diff
  already says what.
- Sign off your commits (`git commit -s`), which adds a `Signed-off-by:` line.
  It is the [Developer Certificate of Origin](https://developercertificate.org/):
  you are stating you have the right to submit the work under this project's
  license. There is no CLA.
- Use a real name and a working email in `Author:`. Some of our own early history
  has the email in the name field; do not copy that.
- Contributions are accepted under Apache-2.0 (see `LICENSE` §5). If you are
  contributing a change to `gpudirect/patches/`, note that those go upstream to
  the project they patch, under *that* project's license.

## Layout

| Path | What it is |
|---|---|
| `lmcache_daos/` | the package: in-process connector, `mp/` adapter, `gds_backend.py` |
| `tests/` | unit tests (no DAOS) and integration tests (cluster required) |
| `bench/` | measurement harnesses |
| `deploy/` | launchers, configuration, operating notes |
| `gpudirect/` | patches and build procedure for the experimental GPU-direct stack |
| `doc/` | design records, measurements, upstream submission drafts |

The three backends are at different maturities and the GPU-direct one is
**experimental**; the README section "모드별 성숙도" says what that means before
you build anything on it.

## Contact

Open an issue for anything public. For a security report, do not use the issue
tracker — see `SECURITY.md`.
