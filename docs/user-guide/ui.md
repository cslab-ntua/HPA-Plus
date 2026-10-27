# Local UI

`make ui` starts a small read-only web page for your HPAPlus objects on your own machine. It shows, for each
autoscaler, how many pods it runs and wants, why it chose that number, and the recent CPU, pod and forecast
history behind the decision. It is part of the repository, not of the Helm chart: nothing is installed in the
cluster.

```sh
make ui
# HPA+ UI on http://hpaplus.localtest.me:8080 (read-only; Ctrl-C to stop)
```

Open the printed address. `*.localtest.me` is a public DNS name that always resolves to your own machine
(`127.0.0.1` and `::1`), so no hosts-file change is needed. The program listens on loopback only. Like the other
`make` targets it needs the Go version declared in `go.mod`; the first start compiles it, later starts reuse the
build cache.

## What it reads

The UI uses your current kubeconfig context, the same one `kubectl` uses, and only sends `get` and `list` requests:

| Object | Used for |
| --- | --- |
| HPAPlus | The overview, the latest decision, model states and checks. |
| The scale target (for example a Deployment) | Pods now and ready pods on an autoscaler's page. |
| ConfigMap `hpa-plus-<name>-data` and its chunks | Stored CPU history, pod counts and the newest cached forecast. |
| Request-demand state ConfigMaps | Requests per second, response time, failed requests and the request-demand forecast. |

Your account needs `get`/`list` on `hpapluses.hpa.plus`, `get` on the scale targets and `get` on ConfigMaps in the
namespaces you want to see. History written by another HPA+ version is still shown; fields the UI does not know
are skipped. The UI never writes to the cluster.

## Options

| Variable | Default | Meaning |
| --- | --- | --- |
| `UI_PORT` | `8080` | Local port. |
| `UI_HOST` | `hpaplus.localtest.me` | Host name the page is opened with. Any `*.localtest.me` name works. |
| `UI_NAMESPACE` | empty | Show one namespace only. Use it when your account may not list HPAPlus in every namespace. |

For example `make ui UI_PORT=9090 UI_NAMESPACE=shop`. To look at another cluster, switch context with
`kubectl config use-context` or run `KUBECONFIG=/path/to/config make ui`.

## Reading the page

The overview groups autoscalers by namespace and marks each one:

- **Healthy**: decisions are current and nothing is reported wrong.
- **Learning**: a model is still collecting the samples it needs.
- **Partial metrics**: some pods have no CPU numbers yet, which is normal for a few minutes after new pods start.
  It becomes **Needs attention** after three minutes.
- **Needs attention**: a check failed, a model is failing, or Kubernetes rejected a change.
- **Idle**: there was no decision for five minutes. The operator may be stopped, or it cannot reconcile the object;
  its logs say which.

An autoscaler's page explains the latest decision in plain words, including limits such as the minimum, maximum,
scale-down stabilization or rate limits. Its charts share one time axis: CPU used against the capacity the pods
have at their target utilization, the pods over time, and the newest forecast in the shaded future area. A
request-demand model adds requests per second, response time and failed requests. HPA+ keeps only its latest
decision, so the decision list starts empty and grows while the page is open.

## Security

The server answers only to the host names it was started for, `localhost`, `127.0.0.1` and `::1`. Requests with any
other `Host` header are refused, so a web page that points its own domain at your machine cannot read the cluster
through your credentials. The page loads no external scripts, styles or fonts, and the server accepts only `GET` and
`HEAD`.
