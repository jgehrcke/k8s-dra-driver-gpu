**Warning:** the test suite runs _invasively_ against the Kubernetes cluster that your local `kubectl` is currently configured against.

## Usage

Invoke `make bats-tests` in the root of this repository.



### Test local dev state (artifacts not pushed)

Not yet supported.
Let's change this ASAP.

This test suite for now assumes public availability of a Helm chart on GHCR or NGC, pointing to a container image publicly available on GHCR or NGC.

### Test Helm chart from registery

#### Default versions

Say, this is the current local git revision:

```console
$ git rev-parse --short=8 HEAD
e6e1dde4
```

Then the test suite runs with the default configuration, for example:
```console
$ make bats-tests
...
        --env TEST_CHART_REPO="oci://ghcr.io/nvidia/k8s-dra-driver-gpu" \
        --env TEST_CHART_VERSION=25.8.0-dev-e6e1dde4-chart \
        --env TEST_CHART_LASTSTABLE_REPO="oci://ghcr.io/nvidia/k8s-dra-driver-gpu" \
        --env TEST_CHART_LASTSTABLE_VERSION="25.3.2-7020737a-chart" \
        --env TEST_CRD_UPGRADE_TARGET_GIT_REF=e6e1dde4 \
...
12 tests, 0 failures in 166 seconds
```

As you can see, this currently requires a Helm chart corresponding to the local revision to be available on GHCR.

#### Test specific versions

Set the correponding `TEST_*` environment variables before invoking the Makefile target.

For example:

```console
$ export TEST_CHART_VERSION="25.8.0-dev-b823882b-chart"
$ export TEST_CRD_UPGRADE_TARGET_GIT_REF="main"
$ make bats-tests
...
12 tests, 0 failures in 166 seconds
```


## Development

Bats is a workable solution.
Developing new tests might however probe your patience.
Make wise usage of

* [skipping tests](https://bats-core.readthedocs.io/en/stable/writing-tests.html#skip-easily-skip-tests)
* [tagging tests with `bats:focus`](https://bats-core.readthedocs.io/en/stable/writing-tests.html#special-tags)
* [CLI args](https://bats-core.readthedocs.io/en/stable/usage.html) such as `--verbose-run`, `--show-output-of-passing-tests`.


Also, familiarize yourself with bat's [`run`](https://bats-core.readthedocs.io/en/stable/writing-tests.html#run-test-other-commands) command.

Don't skip the section about when [not to use `run`](https://bats-core.readthedocs.io/en/stable/writing-tests.html#when-not-to-use-run).

Take inspiration from [cri-o tests](https://github.com/cri-o/cri-o/tree/81e69a58c7e6ec8699b3bdd8696b1d0e25e32bfb/test).
