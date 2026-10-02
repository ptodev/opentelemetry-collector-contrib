# Golden files

What the processor writes when `TestGoldenFiles` sends it the fixture
telemetry in `fixtures_test.go`, once per detail level. The fixtures have
three services: `checkout`, `cart`, and `payments`. The rule keeps
everything but `payments`, so the files also show that records, scopes, and
resources that match no rule are left out.

| File | Contents |
| --- | --- |
| `traces-<level>.yaml` | 16 spans: 2 services, 4 traces each |
| `logs-<level>.yaml` | 8 log records |
| `metrics-<level>.yaml` | 5 metrics per service, one of each type: gauge, sum, histogram, exponential histogram, and summary |

`<level>` is `sizes`, `keys`, or `full`. Each file is one OTLP export
request, decompressed from the processor's output, in the YAML format of
`pkg/golden`. The `tap.session_id` and `tap.instance` resource attributes
differ between runs, so they're replaced with `normalized`.

The fixtures are generated from fixed seeds, so the files only change when the
fixtures or the processor change. To rewrite them after an expected change:

```sh
go test -run TestGoldenFiles -update .
```
