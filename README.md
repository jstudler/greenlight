# greenlight

`greenlight` is a generic, standalone Go binary that makes Kubernetes custom
resources look reconciled. It watches a configurable set of API group/kind
combinations and writes their **`status`** subresource:

- it sets a **`Ready` condition** — `True` by default, `False` only when a rule
  says so, and
- it fills additional status fields that are computed by **rules written in
  [CEL](https://github.com/google/cel-spec)**.

By default it also **removes every finalizer** from the objects it watches.

The purpose is to run an operator in a kind, CI or dev cluster **without** the
downstream operators it depends on. greenlight fakes those controllers by
making their custom resources look reconciled, so the operator under test can
make progress.

greenlight itself contains no domain knowledge: everything specific to an API
lives in a rules file. Two examples ship with it:

| file | what it covers |
|---|---|
| [`examples/netbox.rules.yaml`](examples/netbox.rules.yaml) | the public [netbox-operator](https://github.com/netboxlabs/netbox-operator) `PrefixClaim` and `IpAddressClaim` |
| [`examples/generic.rules.yaml`](examples/generic.rules.yaml) | a fictional `example.com` API, demonstrating every feature of the rules format |

- Module: `github.com/jstudler/greenlight`
- Image: `ghcr.io/jstudler/greenlight`
- Licence: AGPL-3.0 (see [LICENSE](LICENSE))

## Table of contents

- [Quick start](#quick-start)
- [How it works](#how-it-works)
- [Command line](#command-line)
- [Rules file](#rules-file)
  - [Schema](#schema)
  - [CEL environment](#cel-environment)
  - [Built-in functions](#built-in-functions)
  - [Pitfalls](#pitfalls)
  - [Rule selection](#rule-selection)
- [Failure injection](#failure-injection)
- [Finalizers](#finalizers)
- [Allocation model and its limits](#allocation-model-and-its-limits)
- [Observability](#observability)
- [Running in a cluster](#running-in-a-cluster)
- [Development](#development)

## Quick start

Run it locally against the current kube context:

```sh
greenlight \
  --watch 'netbox.dev/PrefixClaim,netbox.dev/IpAddressClaim' \
  --rules examples/netbox.rules.yaml \
  --param defaultIPv4Pool=10.0.0.0/8
```

Check what it would watch and which rule applies to which kind, without
touching anything:

```sh
greenlight --rules examples/netbox.rules.yaml --validate
```

See what it would write, without persisting it:

```sh
greenlight --rules examples/netbox.rules.yaml --dry-run --log-level debug
```

If `--watch` is omitted, the watch set is derived from the `match` blocks of
the rules, so `--rules` alone is usually enough.

## How it works

For every add and update event of a watched object:

1. If `--remove-finalizers` is on (the default), all finalizers are stripped.
2. Objects that are being deleted are never patched.
3. The most specific matching rule is selected; if there is none, a default
   rule applies that only sets the ready condition to `True`.
4. The rule's `matchConditions` are evaluated. All must be true.
5. The rule's variables, status fields and conditions are evaluated.
6. The result is compared with the live object. **Only a real difference causes
   a patch**, which is why greenlight's own updates do not create a loop.
7. The status fields and all managed conditions are written in a **single
   server-side apply**, so a consumer can never see `Ready=True` without the
   fields that go with it.

Because the computed values are deterministic (hash based), a steady state is
reached after one apply.

Watch patterns are re-resolved every `--rediscovery-interval`, so CRDs that are
installed later are picked up automatically, and informers for CRDs that
disappear are stopped.

## Command line

Every flag can also be set through an environment variable named
`GREENLIGHT_<FLAG_IN_UPPER_SNAKE_CASE>`, for example
`GREENLIGHT_REMOVE_FINALIZERS=false`. The flag always wins.

| flag | default | description |
|---|---|---|
| `--kubeconfig` | `$KUBECONFIG`, then in-cluster | standard loading rules |
| `--context` | current | kube context |
| `--watch` | — | repeatable and comma-separated. Patterns: `group/Kind`, `group/*`, `group/version/Kind`. Use `core/Kind` for the core group. Derived from the rules when omitted. |
| `--rules` | — | path to a rules file; repeatable, concatenated in order |
| `--param` | — | repeatable `key=value`, parsed as a YAML scalar; overrides the rules file |
| `--namespace` | all | repeatable; restricts namespaced kinds |
| `--label-selector` | — | applied to every informer |
| `--condition-type` | `Ready` | type of the managed ready condition |
| `--reason` | `Greenlit` | default reason when the ready condition is `True` |
| `--false-reason` | `NotGreenlit` | default reason when a rule sets it to `False` |
| `--message` | `Status set by greenlight` | default ready message |
| `--remove-finalizers` | `true` | strip all `metadata.finalizers` from every watched object |
| `--field-manager` | `greenlight` | server-side apply field manager |
| `--workers` | `4` | workqueue workers |
| `--delay` | `0s` | artificial delay before patching, to simulate a slow backend |
| `--resync-period` | `10m` | informer resync |
| `--rediscovery-interval` | `30s` | how often watch patterns are re-resolved; `0` disables it |
| `--dry-run` | `false` | evaluate and log, apply with `dryRun=All` |
| `--validate` | `false` | compile the rules, resolve the watches, print a table, exit |
| `--qps` / `--burst` | `50` / `100` | client rate limits |
| `--metrics-bind-address` | `:8080` | Prometheus endpoint; `0` disables it |
| `--health-probe-bind-address` | `:8081` | `/healthz` and `/readyz`; `0` disables them |
| `--leader-elect` | `false` | Lease-based leader election |
| `--leader-election-namespace` | pod namespace | |
| `--leader-election-id` | `greenlight` | name of the Lease |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error` |
| `--log-format` | `text` | `text` or `json`; info and debug go to stdout, warn and error to stderr |
| `--version` | | print the version |

A watched resource is only used when it supports `list` and `watch` **and** has
a `/status` subresource. Anything else is skipped with a warning.

## Rules file

### Schema

```yaml
apiVersion: statuspatcher.dev/v1alpha1   # file format version
kind: GreenlightConfig
params:                                  # arbitrary values, exposed to CEL as `params`
  defaultIPv4Pool: 10.0.0.0/8
defaults:                                # optional, overrides the CLI defaults
  readyCondition:
    reason: Greenlit
    falseReason: NotGreenlit
    message: Status set by greenlight
rules:
  - name: netbox-prefixclaim             # required, unique, DNS label
    match:
      apiGroup: netbox.dev               # required; "" or "core" for the core group
      kinds: ["PrefixClaim"]             # required; "*" is allowed
      versions: ["v1"]                   # optional; default is any version
    matchConditions:                     # optional; all must evaluate to true
      - name: not-opted-out
        expression: "object.metadata.?annotations[?'statuspatcher.dev/skip'].orValue('') != 'true'"
    delay: 0s                            # optional per-rule delay
    variables:                           # optional, lazily evaluated and memoised
      - name: key
        expression: "object.metadata.namespace + '/' + object.metadata.name"
    status:                              # status field name -> CEL expression
      prefixName: "object.metadata.name"
    readyCondition:                      # optional; type comes from --condition-type
      statusExpression: "true"           # CEL -> bool; default true. false -> status False
      reason: PrefixClaimResourceReady   # literal, used when True
      falseReason: PrefixClaimResourceNotReady  # literal, used when False
      message: Prefix assigned           # literal
      messageExpression: ""              # optional CEL -> string; a non-empty result wins
    conditions:                          # optional extra managed conditions
      - {type: PrefixAssigned, reason: PrefixCRCreated, falseReason: PrefixCRNotCreated, message: ""}
```

Rules:

- `status` keys are **top-level** status field names. Produce nested objects by
  returning a CEL map. The keys `conditions` and `observedGeneration` are
  **reserved**.
- `status`, `variables[*].expression`, `matchConditions[*].expression`,
  `statusExpression` and `messageExpression` are **CEL**.
- `type`, `reason`, `falseReason` and `message` are **literals**, following the
  `message` versus `messageExpression` convention of
  ValidatingAdmissionPolicy.
- `reason` and `falseReason` must match `^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`:
  **no spaces and no dashes**, because the API server enforces that.
- `statusExpression` must return a `bool`. `Unknown` is not supported in
  `v1alpha1`.
- An extra condition's `type` must differ from `--condition-type`.
- YAML anchors (`&`), aliases (`*`) and merge keys (`<<:`) are supported and
  used by the shipped examples.

Everything is validated and compiled at startup: a broken rules file fails
immediately instead of misbehaving later.

### CEL environment

Three root variables are available, all dynamically typed, exactly like a
ValidatingAdmissionPolicy:

| variable | content |
|---|---|
| `object` | the whole object, including its current `status` |
| `params` | the merged parameters from the rules files and `--param` |
| `variables` | the rule's own variables, evaluated lazily and memoised |

Enabled libraries: optional types (`?.`, `[?..]`, `orValue`, `optional.of`,
`optional.none`), `strings`, `lists`, `sets`, `math`, `encoders`, plus the
Kubernetes `ip`, `cidr`, `format`, `lists`, `regex`, `quantity` and `url`
libraries.

Each expression has a cost limit, so a pathological rule cannot stall a worker.

### Built-in functions

All of them take and return plain strings, support IPv4 and IPv6, and are
deterministic.

| function | semantics |
|---|---|
| `hash(string) -> int` | FNV-1a 64, masked to a non-negative int64 |
| `cidrSubnet(parent, prefixLength, index) -> string` | the `index`-th subnet of that length inside `parent`; always masked |
| `cidrHost(prefix, index) -> string` | the `index`-th address of `prefix` as a bare IP (`0` is the network address) |
| `pickSubnet(parent, prefixLength, key) -> string` | a deterministic subnet chosen by `hash(key)` |
| `pickHost(prefix, key) -> string` | a deterministic **usable** host: IPv4 of `/30` or shorter excludes the network and broadcast addresses, IPv6 of `/126` or shorter excludes index 0; `/31`, `/32`, `/127` and `/128` use every address |
| `hostCIDR(ip) -> string` | `ip + "/32"` or `ip + "/128"` |

Invalid input or an out-of-range argument raises a CEL error, which omits that
one field rather than failing the object.

### Pitfalls

- **Literal strings need inner quotes.** `lastOperationStatus: "'COMPLETED'"`
  is the string `COMPLETED`; `"COMPLETED"` is an identifier and fails to
  compile.
- **Ternaries must not mix types.** `cond ? 'x' : null` fails type checking.
  Use `optional.of('x')` and `optional.none()`, or wrap a branch in `dyn(..)`.
- **Optional fields.** An expression that evaluates to `optional.none()` or
  `null` omits the field. Server-side apply then removes it again if greenlight
  owned it before, which is intentional.
- **Integers stay integers.** `params` is decoded so that whole numbers remain
  `int64`; otherwise `params.base + variables.id` would fail with
  `no such overload double + int`.
- **Use optional selection for fields that may be absent.**
  `object.spec.?parentPrefix.orValue('')` never fails, `object.spec.parentPrefix`
  does when the field is unset.

### Rule selection

For an object of `group/version/Kind`, candidate rules are those whose
`apiGroup` matches, whose `kinds` contains the kind or `*`, and whose
`versions` is empty or contains the version. They are ordered by specificity —
exact kind and version, then exact kind, then `*` with a version, then `*` —
and the **first** one applies. Rules are never merged.

When nothing matches, a default rule sets only the ready condition to `True`.
`--validate` prints the resulting table and warns about rules that match
nothing.

## Failure injection

The shipped examples honour two annotations, which make negative end-to-end
tests easy:

| annotation | effect |
|---|---|
| `statuspatcher.dev/skip: "true"` | greenlight leaves the object completely alone |
| `statuspatcher.dev/ready: "false"` | the ready condition becomes `False`, with the rule's `falseReason` |

Flipping an annotation changes the computed status without bumping
`metadata.generation`, and the diff check notices it, so the object is patched
right away.

## Finalizers

With `--remove-finalizers` (the default) greenlight strips **all** entries of
`metadata.finalizers` from **every** watched object, whether it is terminating
or not, and regardless of who added them. The patch is optimistically locked on
the `resourceVersion`, so a concurrent change causes a requeue instead of
clobbering somebody else's update.

> **Caveat.** If another controller keeps adding a finalizer to a watched kind,
> the two controllers will keep undoing each other. A typical example is an
> operator that adds deletion-protection finalizers to the very resources
> greenlight fakes. Run greenlight with `--remove-finalizers=false` for those
> scenarios, and delete the leftover objects manually or with
> `kubectl patch --type=merge -p '{"metadata":{"finalizers":null}}'`.

## Allocation model and its limits

Allocation is **hash only**: values derive from an FNV-1a hash of
`namespace/name`. That makes them stable across restarts and across
delete/recreate cycles, so end-to-end tests can hard-code them.

There is, however, **no uniqueness guarantee across objects**. Two names can
hash into the same subnet or host address. For example, three IP claims sharing
one `/25` (126 usable hosts) collide with a probability of roughly 2.4 %. The
result is deterministic, so for a given set of names it either always happens
or never does.

The workaround is to mix a salt into the key. `examples/netbox.rules.yaml`
already supports it:

```sh
greenlight --rules examples/netbox.rules.yaml --param salt=run-2/
```

## Observability

Prometheus metrics on `--metrics-bind-address`:

| metric | labels |
|---|---|
| `greenlight_patches_total` | `group`, `kind`, `result` (`applied`, `unchanged`, `error`, `dryrun`) |
| `greenlight_ready_status_total` | `group`, `kind`, `status` |
| `greenlight_evaluation_errors_total` | `rule`, `path` |
| `greenlight_finalizers_removed_total` | `group`, `kind` |
| `greenlight_watched_resources` | — |
| `greenlight_reconcile_duration_seconds` | `group`, `kind` |

`/healthz` is always green once the process is up. `/readyz` turns green once
the watch patterns have been resolved and every informer has synced.

Evaluation errors **never block readiness**:

| failing expression | behaviour |
|---|---|
| `matchConditions[*].expression` | treated as **true**, the object is processed |
| `variables[*]` | only the fields that use it fail |
| `status.<field>` | that field is **omitted**, the others are written |
| `statusExpression` | the condition stays **True** |
| `messageExpression` | the literal `message` is used |

Every error is logged once per `--resync-period` per object and expression, so
a resync does not flood the log. No Kubernetes Events are emitted.

A status field that the CRD schema does not declare is a special case. Unlike a
plain update, server-side apply rejects such a payload outright, so greenlight
drops the offending field, logs it at **warn** level and remembers it for the
rest of the process lifetime. The ready condition is still written.

## Running in a cluster

```sh
make deploy-kind IMAGE=ghcr.io/jstudler/greenlight KIND_CLUSTER=kind
```

That builds the image, loads it into the kind cluster and applies
[`config/default`](config/default), which creates the `greenlight-system`
namespace, a ServiceAccount, RBAC, a ConfigMap with the rules and a Deployment
with one replica, leader election, a restricted security context and probes on
`:8081`.

The rules are mounted at `/etc/greenlight`. kustomize refuses to read files
from outside its own directory, so the deployment copies live in
[`config/manager/rules/`](config/manager/rules); `make sync-rules` refreshes
them from `examples/` and `make verify-rules` fails if they have drifted.

RBAC follows this pattern, per watched API group:

- `get`, `list`, `watch` on the resources
- `patch` on `<resource>/status`
- `patch` on the resources themselves — only needed with
  `--remove-finalizers=true`, which is the default
- `get`, `create`, `update` on `coordination.k8s.io/leases` in the deployment
  namespace, only with `--leader-elect`

Discovery is already covered by the default `system:discovery` ClusterRole, and
no permissions on events are needed. Edit
[`config/rbac/role.yaml`](config/rbac/role.yaml) to match your `--watch` flags.

## Development

```sh
make help              # list every target
make build             # build bin/greenlight
make test              # unit tests
make test-integration  # envtest integration tests (downloads the control plane binaries)
make lint              # golangci-lint
make validate-rules    # compile the example rules without a cluster
make validate-examples # resolve the example rules against the current cluster
```

Layout:

```
cmd/greenlight/        entry point
internal/app/          wiring: clients, servers, leader election, --validate
internal/options/      flags, environment variables, validation
internal/rules/        loading, validation, CEL compilation and evaluation
internal/celfuncs/     the deterministic CEL helper functions
internal/watch/        watch patterns and discovery resolution
internal/patcher/      informers, workqueue, diff, conditions, apply, finalizers
internal/metrics/      Prometheus collectors
internal/logging/      structured logging
examples/              example rules files
config/                kustomize manifests
test/testdata/crds/    minimal CRDs used by the integration tests
test/integration/      envtest suite
```

The CRDs under `test/testdata/` are **minimal synthetic fixtures** that model
only the fields greenlight reads or writes. They exist so the integration tests
can run against a real API server without installing any operator.
