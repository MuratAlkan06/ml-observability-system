# deploy/eks

The D39 demo kit (Phase 3 H2): an owner-run demonstration, never CI, that
installs the operator's Helm chart on an ephemeral EKS cluster and deletes the
cluster in the same run. `cluster.yaml` is the eksctl config, rendered per run;
`demo.sh` does `run`, `sweep` and `teardown`, and its header is the usage, from
the one-command invocation down; `test/run.sh` is the stub harness, which runs
`demo.sh` against fakes of aws, eksctl, kubectl, helm, terraform and curl, with
no network and no AWS account. The evidence is on issue
[#92](https://github.com/MuratAlkan06/ml-observability-system/issues/92).
